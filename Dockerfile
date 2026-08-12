# rule110 — Rule 110 as a Bitcoin Script covenant.
#
# Two stages: a toolchain image that compiles, and a base with nothing in it
# that runs. See deploy/k8s/ for the manifests, and README.md for the ordered
# runbook — `run` is only the last step of five.

# ---- build ------------------------------------------------------------------

# Alpine, so the C toolchain is musl and the result can be linked statically.
# See the build step below for why there is a C toolchain here at all.
FROM golang:1.26-alpine AS build

RUN apk add --no-cache build-base

WORKDIR /src

# Dependencies before sources, so editing Go code does not re-resolve the module
# graph. Nothing here needs a checkout on disk: go.mod's three replaces point at
# public git hosts, which is the whole reason they were changed from absolute
# paths under /git.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=1, which is not what the dependency list suggests.
#
# The database drivers are indeed pure Go — modernc.org/sqlite is transpiled C
# with no libc dependency, and pgx never had one — so neither of them needs cgo.
# The requirement comes from somewhere else: this application compiles the Rúnar
# contract to Bitcoin Script at RUNTIME, out of the source embedded by
# contracts/embed.go, so the binary carries the Rúnar compiler, whose frontend
# parses Go with tree-sitter, which is a C library. CGO_ENABLED=0 does not
# produce a smaller image here, it fails the build:
#
#   github.com/smacker/go-tree-sitter/iter.go:17:18: undefined: Node
#
# The alternative is to compile the contract at image-build time and embed the
# artifact instead of the source, which would drop the compiler and the C
# toolchain with it. That is a change to the application, not to its packaging,
# so it is not made here.
#
# Linking statically against musl still gets what the runtime stage wants: one
# self-contained file with no libc to ship. osusergo and netgo replace the two
# remaining things that would otherwise reach for the system C library.
#
# -s -w strips the symbol table and DWARF, which is most of the difference
# between a 39 MB binary and a 55 MB one. It also costs pprof its symbol names:
# a CPU profile taken from a stripped build reports addresses where it should
# report functions. That is the right trade for the image that runs all the
# time, and the wrong one for a measurement run — hence the debug stage below,
# which is the same build with the stripping removed. LDFLAGS carries the
# difference so there is only ever one build command to keep correct.
ARG LDFLAGS="-s -w -extldflags=-static"
RUN CGO_ENABLED=1 GOOS=linux go build \
      -trimpath \
      -tags osusergo,netgo \
      -ldflags="${LDFLAGS}" \
      -o /out/rule110 ./cmd/rule110

# Fail here rather than in a crash loop: a dynamically linked binary builds
# perfectly and then cannot start on a base image with no loader.
RUN if ldd /out/rule110 2>/dev/null | grep -q '=>'; then \
      echo "rule110 is dynamically linked; it will not run on distroless/static" >&2; \
      exit 1; \
    fi

# ---- runtime ----------------------------------------------------------------

# Distroless static: ca-certificates (arcade is reached over HTTPS) and nothing
# else. No shell, no package manager, no coreutils.
#
# The absence of a shell is a deliberate trade and it changes how the bootstrap
# is done: you cannot `kubectl exec -- sh` into this. Run the subcommands
# directly instead — `kubectl exec … -- rule110 address` works, because exec
# needs a binary, not a shell — or use the one-shot Job in
# deploy/k8s/bootstrap-job.yaml.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# 65532 is distroless's "nonroot" user. It is named explicitly because the data
# directory has to be writable BY THIS UID and nothing enforces that for you:
#
#   chain.LoadOrCreateIdentity does os.MkdirAll(dir, 0700) and, if keys.json is
#   not there, GENERATES A NEW WALLET and writes it. On an unwritable mount that
#   write fails and the process exits, which is the loud case. The quiet case is
#   worse: if the directory is writable but EMPTY — the volume was not mounted,
#   or a fresh PVC was provisioned under an existing deployment — startup
#   succeeds, a brand-new keypair is created, the funding address changes, and
#   the coins held by the previous wallet become unreachable. There is no
#   restore-from-seed in an arcade-only wallet. Nothing recovers them.
#
# So: mount a persistent volume at /data, make sure it is the SAME volume across
# restarts, and give it to this UID (the StatefulSet does that with
# fsGroup: 65532). Back it up.
USER 65532:65532

COPY --from=build /out/rule110 /usr/local/bin/rule110

# RULE110_DATA_DIR points at the mount rather than the ./data default, which
# would land on the container's ephemeral layer and be lost on every restart —
# see above for what that costs. RULE110_ADDR binds all interfaces because
# nothing outside the container can reach a loopback listener.
ENV RULE110_DATA_DIR=/data \
    RULE110_ADDR=0.0.0.0:8110

EXPOSE 8110

# No VOLUME instruction: it would make `docker run` silently invent an anonymous
# volume, which looks like persistence and is not. The mount is declared where
# it can be reviewed — in the StatefulSet.

ENTRYPOINT ["/usr/local/bin/rule110"]
CMD ["run"]

# ---- debug ------------------------------------------------------------------

# The same image, built with symbols, for a measurement run.
#
#   docker build --target debug --build-arg LDFLAGS=-extldflags=-static -t rule110:debug .
#
# The build arg is what drops -s -w; without it this stage is just the default
# image under another name, which would be a trap rather than a convenience.
# A profile from a stripped binary still collects, it just reports addresses
# instead of function names, and discovering that after the run is over is the
# specific waste this stage exists to prevent.
#
# It is a separate target rather than the default because the stripping is worth
# having in the image that runs continuously: profiling costs a few percent even
# when nobody is scraping it, and the symbols are 16 MB of image nobody reads.
#
# Reaching it still needs -pprof, which is off unless set. The listener defaults
# to loopback, so exposing it here would be pointless: set RULE110_PPROF to a
# bindable address and port-forward to it.
FROM runtime AS debug

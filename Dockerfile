# Two stages: build with the full Go toolchain, ship only the program.
#
# Stage 1 has a compiler, a shell and Debian (about 900 MB). Stage 2 copies just
# the compiled binary and the web folder onto "distroless": no shell, no package
# manager, nothing to log in to. The final image is ~20 MB and has very little
# for an attacker to use, or for a vulnerability scanner to flag.

# ---------- stage 1: build ----------
# Keep this version in step with the "go" line in go.mod.
FROM golang:1.27.2 AS build
WORKDIR /src

# Dependencies first, in their own layer. Docker caches each step and reruns it
# only if its inputs changed, so editing Go code doesn't re-download modules.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
# The *.up.sql files are compiled into the binary (migrations/embed.go) for `api migrate`.
COPY migrations/ ./migrations/
# CGO_ENABLED=0: pure Go, no C library, so the binary runs on an image with no libc.
# -trimpath: no build-machine paths inside the binary.
# -ldflags="-s -w": drop debug symbols (smaller file; stack traces still work).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# ---------- stage 2: run ----------
# "static" = for self-contained binaries; includes CA certificates (needed for
# HTTPS calls to the email provider) and time zone data. ":nonroot" runs as an
# unprivileged user (uid 65532) instead of root.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/api ./api
# The server serves ./web relative to its working directory (/app).
COPY web/ ./web/

# Documentation only: Cloud Run sets PORT, and main.go listens on it (default 8080).
EXPOSE 8080
ENTRYPOINT ["/app/api"]

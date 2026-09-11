FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server ./cmd/server

# distroless/static over scratch: it brings exactly what scratch is missing
# for this app — CA certificates (the HTTPS call to Resend needs a root
# store), tzdata (so time.Local isn't always UTC), a writable /tmp, and a
# non-root default user — while still shipping no shell and no package
# manager.
#
# The image is meant to be run under whatever uid:gid already owns the data
# on the host (`--user`), and the tag's own uid is not something anything
# outside this file should depend on. The binary is static and looks no
# account up, so any uid works with nothing to create in the image. That
# user needs write access to DB_PATH and COVERS_DIR, and only read access to
# LIBRARY_DIR, on whatever volumes are mounted over them.
FROM gcr.io/distroless/static-debian13:nonroot

# The nonroot image sets WORKDIR /home/nonroot. cmd/server's defaults
# (/library, /data/library.db, /data/covers) are absolute, so this is only
# what a relative path given in the environment resolves against — / and not
# a home directory nothing is ever mounted over.
WORKDIR /
COPY --from=build /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]

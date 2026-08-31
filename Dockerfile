FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server ./cmd/server

# distroless/static over scratch: it brings exactly what scratch is missing
# for this app — CA certificates (the HTTPS call to Resend needs a root
# store), tzdata (so time.Local isn't always UTC), a writable /tmp, and a
# non-root "nonroot" user (uid 65532) — while still shipping no shell and no
# package manager. DB_PATH, COVERS_DIR and LIBRARY_DIR must be writable by
# uid 65532 on whatever volumes are mounted over them.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]

# --- build : binaire statique, sans dépendance ---
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/zedcode .

# --- runtime : image vide, utilisateur non-root, aucune écriture disque ---
FROM scratch
COPY --from=build /out/zedcode /zedcode
USER 65532:65532
ENV ADDR=:8080 GOMEMLIMIT=48MiB
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["/zedcode", "-healthcheck"]
ENTRYPOINT ["/zedcode"]

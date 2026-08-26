FROM node:24-alpine AS web
WORKDIR /src
COPY frontend/package*.json frontend/
RUN npm --prefix frontend ci
COPY frontend/ frontend/
RUN npm --prefix frontend run build

FROM golang:1.25-alpine AS build
ARG APP_COMMIT=unknown
ARG APP_VERSION=2.0.0-dev
WORKDIR /src
COPY . .
COPY --from=web /src/frontend/dist/ internal/webui/dist/
RUN CGO_ENABLED=0 GOOS=linux go test ./... && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X icloud-privacy-mail-v2/internal/buildinfo.Version=${APP_VERSION} -X icloud-privacy-mail-v2/internal/buildinfo.Commit=${APP_COMMIT}" -o /out/ipm-server .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/ipm-server /usr/local/bin/ipm-server
VOLUME ["/data"]
EXPOSE 8788
ENTRYPOINT ["/usr/local/bin/ipm-server"]
CMD ["-config", "/data/config.json"]

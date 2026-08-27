# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/convertbox ./cmd/convertbox

FROM alpine:3.22
RUN apk add --no-cache imagemagick poppler-utils ca-certificates && addgroup -S app && adduser -S -G app -u 10001 app
COPY deploy/imagemagick-policy.xml /etc/ImageMagick-7/policy.xml
COPY --from=build /out/convertbox /usr/local/bin/convertbox
USER 10001:10001
EXPOSE 8080
ENV CONVERTBOX_STORAGE=/tmp/convertbox MAGICK_CONFIGURE_PATH=/etc/ImageMagick-7
ENTRYPOINT ["/usr/local/bin/convertbox"]

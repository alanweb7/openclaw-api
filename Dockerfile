FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/openclaw-api .

FROM alpine:3.20
RUN addgroup -S app && adduser -S -G app app && apk add --no-cache ca-certificates wget
WORKDIR /app
COPY --from=build /out/openclaw-api /app/openclaw-api
USER app
EXPOSE 8080
ENV PORT=8080
ENTRYPOINT ["/app/openclaw-api"]

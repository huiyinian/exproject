FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /weekly-contest ./cmd/server
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /weekly-contest /weekly-contest
EXPOSE 8080
ENTRYPOINT ["/weekly-contest"]

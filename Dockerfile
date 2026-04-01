FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum .
RUN go mod download 
COPY . .
RUN GOOS=linux GOARCH=amd64 go build -o /out/ha ./cmd

FROM alpine
RUN apk update && apk add ca-certificates 
WORKDIR /app
COPY --from=build /out/ha /app/ha

CMD ["/app/ha"]

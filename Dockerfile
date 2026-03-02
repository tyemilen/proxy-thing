FROM golang:1.25.7 AS build

WORKDIR /proxy-thing

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bin/proxy-thing main.go

FROM alpine:3.14
COPY --from=build /bin/proxy-thing /bin/proxy-thing
RUN chmod +x /bin/proxy-thing

CMD ["/bin/proxy-thing"]
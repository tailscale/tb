FROM golang:1.21

COPY tb.go tb.go
RUN go build -o /tb tb.go

CMD ["/tb"]

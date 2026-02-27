FROM golang:1.22-alpine AS builder

WORKDIR /chaincode
COPY . .
RUN go build -o payment .

FROM alpine:3.19
COPY --from=builder /chaincode/payment /usr/local/bin/payment
EXPOSE 9999
CMD ["payment"]

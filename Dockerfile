FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o kubearch .

# ---

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /app/kubearch /kubearch

EXPOSE 9101

ENTRYPOINT ["/kubearch"]

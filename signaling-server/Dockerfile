# Docker를 이용한 시그널링 서버 배포 가이드

## Dockerfile
FROM golang:1.21-alpine AS builder

WORKDIR /app

# 의존성 파일 복사
COPY go.mod ./
RUN go mod download

# 소스 코드 복사
COPY signaling-server.go .
COPY sender.html .

# 빌드
RUN go build -o signaling-server signaling-server.go

# 실행 이미지
FROM alpine:latest

WORKDIR /app

# 빌드된 바이너리와 HTML 복사
COPY --from=builder /app/signaling-server .
COPY --from=builder /app/sender.html .

# 포트 노출
EXPOSE 8080

# 실행
CMD ["./signaling-server"]
FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o redx-mailer .

FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

COPY --from=builder /app/redx-mailer .

# Create directories for persistent storage
RUN mkdir -p /data/uploads /data/smtp /data/tasks /data/temp
VOLUME /data

# Environment variables (set these in Render dashboard)
ENV PORT=8080
ENV GOOGLE_SERVICE_ACCOUNT=/root/service-account.json

# Expose port
EXPOSE 8080

# Create startup script
RUN echo '#!/bin/sh' > /root/start.sh && \
    echo 'if [ -n "$GOOGLE_SERVICE_ACCOUNT_JSON" ]; then' >> /root/start.sh && \
    echo '  echo "$$GOOGLE_SERVICE_ACCOUNT_JSON" > /root/service-account.json' >> /root/start.sh && \
    echo 'elif [ -n "$GOOGLE_SERVICE_ACCOUNT" ]; then' >> /root/start.sh && \
    echo '  if [ -f "$$GOOGLE_SERVICE_ACCOUNT" ]; then' >> /root/start.sh && \
    echo '    cp "$$GOOGLE_SERVICE_ACCOUNT" /root/service-account.json' >> /root/start.sh && \
    echo '  else' >> /root/start.sh && \
    echo '    echo "$$GOOGLE_SERVICE_ACCOUNT" > /root/service-account.json' >> /root/start.sh && \
    echo '  fi' >> /root/start.sh && \
    echo 'fi' >> /root/start.sh && \
    echo 'chmod 600 /root/service-account.json' >> /root/start.sh && \
    echo './redx-mailer' >> /root/start.sh && \
    chmod +x /root/start.sh

CMD ["/bin/sh", "/root/start.sh"]
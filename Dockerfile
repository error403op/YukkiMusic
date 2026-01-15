FROM golang:1.25.5-bookworm AS builder

WORKDIR /build

# hadolint ignore=DL3015
RUN apt-get update && \
    apt-get install -y \
        git \
        gcc \
        unzip \
        curl \
        zlib1g-dev && \
    rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod tidy

COPY install.sh ./
COPY . .

RUN chmod +x install.sh && \
    ./install.sh -n --quiet --skip-summary && \
    CGO_ENABLED=1 go build -v -trimpath -ldflags="-w -s" -o app ./cmd/app/


# ---------------- RUNTIME IMAGE ----------------

FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y \
        ffmpeg \
        curl \
        unzip \
        zlib1g \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# SSL certs
COPY --from=builder /etc/ssl/certs /etc/ssl/certs

# -------- yt-dlp --------
RUN curl -fL \
      https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux \
      -o /usr/local/bin/yt-dlp && \
    chmod 0755 /usr/local/bin/yt-dlp


# -------- Node.js (for yt-dlp JS challenges) --------
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - && \
    apt-get install -y nodejs && \
    rm -rf /var/lib/apt/lists/*


# -------- Deno --------
RUN curl -fsSL https://deno.land/install.sh | sh && \
    mv /root/.deno/bin/deno /usr/local/bin/deno && \
    chmod +x /usr/local/bin/deno && \
    rm -rf /root/.deno


# -------- Bun --------
RUN curl -fsSL https://bun.sh/install | bash && \
    mv /root/.bun/bin/bun /usr/local/bin/bun && \
    chmod +x /usr/local/bin/bun && \
    rm -rf /root/.bun


# Verify all JS engines exist
RUN node --version && \
    deno --version && \
    bun --version



# Create non-root user
RUN useradd -r -u 10001 appuser && \
    mkdir -p /app && \
    mkdir -p /home/appuser/.cache/yt-dlp && \
    chown -R appuser:appuser /app /home/appuser

ENV HOME=/home/appuser

WORKDIR /app

COPY --from=builder /build/app /app/app
RUN chown appuser:appuser /app/app

USER appuser

ENTRYPOINT ["/app/app"]

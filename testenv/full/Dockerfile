# Debian 12 with systemd and the software the app manages (nginx, caddy,
# ufw, postgres, mariadb, redis, certbot…). Used by the integration tests.
FROM debian:bookworm
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus openssh-server sudo nginx caddy ufw iptables nftables \
      postgresql mariadb-server redis-server certbot git curl ca-certificates procps \
      iproute2 rsyslog fail2ban locales cron gzip tar unzip less python3 \
    && apt-get clean && rm -rf /var/lib/apt/lists/*
RUN useradd -m -s /bin/bash tester && echo 'tester:secret123' | chpasswd && usermod -aG sudo tester \
    && sed -ri 's/^#?PasswordAuthentication .*/PasswordAuthentication yes/' /etc/ssh/sshd_config \
    && systemctl enable ssh nginx postgresql mariadb redis-server rsyslog \
    && systemctl disable caddy fail2ban || true
STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]

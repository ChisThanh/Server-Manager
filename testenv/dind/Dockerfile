# Docker-in-Docker host reachable over SSH, for the Docker/Compose tests.
FROM docker:27-dind
RUN apk upgrade --no-cache && apk add --no-cache openssh sudo bash shadow git curl coreutils
RUN ssh-keygen -A \
    && adduser -D -s /bin/bash tester \
    && echo 'tester:secret123' | chpasswd \
    && (getent group docker >/dev/null || addgroup -S docker) \
    && addgroup tester docker \
    && echo 'tester ALL=(ALL) ALL' > /etc/sudoers.d/tester \
    && sed -ri 's/^#?PasswordAuthentication .*/PasswordAuthentication yes/' /etc/ssh/sshd_config
COPY start.sh /start.sh
RUN chmod +x /start.sh
ENTRYPOINT ["/start.sh"]

# AlmaLinux + firewalld test server (RHEL family) for the firewalld tests.
FROM almalinux/9-init
RUN dnf install -y firewalld openssh-server sudo iptables-nft procps-ng && dnf clean all \
 && useradd -m tester && echo 'tester:secret123' | chpasswd && usermod -aG wheel tester \
 && systemctl enable firewalld sshd && ssh-keygen -A
CMD ["/usr/sbin/init"]

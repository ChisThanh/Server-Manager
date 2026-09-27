# Test servers

```bash
docker build -t sm-test-full testenv/full
docker run -d --name sm-test-full --privileged --cgroupns=host \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock -p 2225:22 sm-test-full
docker build -t sm-test-dind testenv/dind
docker run -d --name sm-test-dind --privileged -p 2224:22 sm-test-dind
```
A second copy of the full image is used by the security tests (firewall and
sshd changes must not disturb the others):

```bash
docker run -d --name sm-test-sec --privileged --cgroupns=host \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock -p 2226:22 sm-test-full
```

The backup tests use MinIO on 127.0.0.1:9100 (user `smtest`, password
`smtestsecret`):

```bash
docker run -d --name sm-test-minio -p 9100:9000 -e MINIO_ROOT_USER=smtest \
  -e MINIO_ROOT_PASSWORD=smtestsecret cgr.dev/chainguard/minio:latest server /tmp/data
```

User `tester` / `secret123` (sudo with password).

The firewalld tests (`TestAccessFirewalld`) use an AlmaLinux server with
firewalld; they are skipped when it is not running:

```bash
docker build -t sm-test-fwd testenv/fwd
docker run -d --name sm-test-fwd --privileged --cgroupns=host \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw -p 2227:22 sm-test-fwd
```

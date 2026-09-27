#!/bin/sh
/usr/sbin/sshd
exec dockerd-entrypoint.sh dockerd --group docker

FROM ubuntu:24.04
RUN apt-get update && apt-get install -y iptables iproute2 rsync && rm -rf /var/lib/apt/lists/*
COPY orchestrator /usr/bin/orchestrator
RUN chmod +x /usr/bin/orchestrator

ENTRYPOINT [ "/usr/bin/orchestrator"]


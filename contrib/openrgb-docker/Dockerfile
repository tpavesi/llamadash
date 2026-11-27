# OpenRGB command line in a container, for systems that do not package OpenRGB 1.0
# (Debian 12, for one). See the README, "Status lighting".
FROM debian:sid-slim
RUN apt-get update && apt-get install -y --no-install-recommends openrgb && rm -rf /var/lib/apt/lists/*
# openrgb never outlives the dashboard's own 20 second limit
ENTRYPOINT ["timeout", "15", "openrgb"]

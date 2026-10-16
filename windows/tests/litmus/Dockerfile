# litmus WebDAV compliance suite, run against an ncwin install:
#   docker build -t ncwin-litmus windows/tests/litmus
#   docker run --rm ncwin-litmus https://host.docker.internal:18443/remote.php/dav/files/admin/ admin <password>
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends litmus ca-certificates && rm -rf /var/lib/apt/lists/*
# Nextcloud's upstream CI runs the same suites (see .github/workflows/integration-litmus.yml).
# largefile is not packaged by Debian; windows/tests TestChunkedUpload covers large files.
ENV TESTS="basic copymove props"
ENTRYPOINT ["litmus", "-k"]

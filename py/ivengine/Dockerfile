# The IV engine: python:3.12 with the locked dependencies; build from the repo root
# (docker build -f py/ivengine/Dockerfile .) so config/ is in the context.
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.26 /uv /usr/local/bin/uv
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PROJECT_ENVIRONMENT=/opt/venv UV_PYTHON_DOWNLOADS=never
WORKDIR /src
COPY py/ivengine/pyproject.toml py/ivengine/uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project
COPY py/ivengine/ ./
RUN uv sync --frozen --no-dev --no-editable \
 && useradd -u 10001 -M -s /usr/sbin/nologin pmt
WORKDIR /app
COPY config/ /app/config/
ENV PATH=/opt/venv/bin:$PATH TZ=Asia/Kolkata PMT_CONTAINER=1 PMT_CONFIG=/app/config/pmt.yaml PYTHONUNBUFFERED=1
USER pmt
CMD ["ivengine"]

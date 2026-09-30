FROM python:3.12-slim
WORKDIR /app
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY app ./app
# Shared taxonomy, read at runtime (compose also mounts ./infra over this so
# edits show up without a rebuild).
COPY --from=infra taxonomy.json /app/infra/taxonomy.json
ENV TAXONOMY_PATH=/app/infra/taxonomy.json
EXPOSE 8000
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8000", "--reload"]

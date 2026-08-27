FROM python:3.11-slim
WORKDIR /app
COPY requirements.txt .
# Optuna has no torch/CUDA dependency (unlike rl-service, CLAUDE.md §15.1's Dockerfile note) — a
# plain pip install is enough, no CPU-only-wheel workaround needed here.
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
EXPOSE 8001
CMD ["uvicorn", "optimizer_service.api:app", "--host", "0.0.0.0", "--port", "8001"]

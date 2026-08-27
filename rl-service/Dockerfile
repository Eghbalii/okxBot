FROM python:3.11-slim
WORKDIR /app
COPY requirements.txt .
# CPU-only torch build: the default PyPI wheel pulls in the full CUDA/nvidia-* toolkit (several
# GB) which is dead weight on a box with no GPU (and this service never uses one) — install torch
# from the CPU-only index first so requirements.txt's plain `torch>=2.2` is already satisfied and
# pip doesn't fall back to downloading the CUDA-enabled wheel.
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu \
    && pip install --no-cache-dir -r requirements.txt
COPY . .
EXPOSE 8000
CMD ["uvicorn", "rl_service.serve.api:app", "--host", "0.0.0.0", "--port", "8000"]

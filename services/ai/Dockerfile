FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    AI_GRPC_ADDR=0.0.0.0:9094
WORKDIR /app
COPY services/ai/requirements.txt /app/services/ai/requirements.txt
RUN pip install --no-cache-dir -r /app/services/ai/requirements.txt
COPY proto /app/proto
RUN mkdir -p /app/services/ai/generated && \
    python -m grpc_tools.protoc -I /app/proto -I /usr/include \
      --python_out=/app/services/ai/generated --grpc_python_out=/app/services/ai/generated \
      /app/proto/finsights/v1/ai.proto && \
    find /app/services/ai/generated -type d -exec touch {}/__init__.py \;
COPY services/__init__.py /app/services/__init__.py
COPY services/ai/__init__.py /app/services/ai/__init__.py
COPY services/ai/app.py /app/services/ai/app.py
USER 10001:10001
EXPOSE 8081 9094
CMD ["uvicorn", "services.ai.app:app", "--host", "0.0.0.0", "--port", "8081"]

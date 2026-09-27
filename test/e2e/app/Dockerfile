FROM python:3.13-alpine
ARG APP_VERSION=v1
ENV APP_VERSION=$APP_VERSION PYTHONUNBUFFERED=1
COPY app.py /app.py
CMD ["python", "/app.py"]

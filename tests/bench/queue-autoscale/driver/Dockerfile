# syntax=docker/dockerfile:1.7
FROM python:3.12-slim
WORKDIR /app
RUN pip install --no-cache-dir httpx==0.27.0 redis==5.0.7 psycopg[binary]==3.2.3 urllib3==2.2.3
COPY bench-driver.py /app/bench-driver.py
ENTRYPOINT ["python", "/app/bench-driver.py"]

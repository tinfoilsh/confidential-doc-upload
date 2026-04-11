FROM python:3.12-slim

RUN pip install --no-cache-dir \
    pymupdf \
    python-docx \
    python-pptx \
    beautifulsoup4 \
    openpyxl \
    fastapi \
    uvicorn \
    python-multipart

COPY app.py /app/app.py
WORKDIR /app

EXPOSE 5002
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "5002", "--workers", "2"]

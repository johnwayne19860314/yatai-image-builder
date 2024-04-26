
FROM python:3.8-slim

# RUN python3 -m venv venv
# RUN venv/bin/pip3 install --upgrade pip
# RUN pip3 install twine

WORKDIR /app

#COPY temp ./temp
COPY README.md ./model/README.md
COPY bentofile.yaml ./model/bentofile.yaml
COPY requirements.txt ./model/requirements.txt
COPY service.py ./model/service.py
COPY train.py ./model/train.py

RUN pip install -r ./model/requirements.txt


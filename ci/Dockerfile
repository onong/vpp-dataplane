FROM ubuntu:20.04

ADD vppdev.sh /usr/bin/calivppctl
ADD run_custom_tests.sh /root/run_custom_tests.sh
ADD calivppctl.py /root/calivppctl.py

RUN apt-get update && apt-get install -y curl python3 pip unzip \
    && curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" \
    && install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl \
    && curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip" \
    && unzip awscliv2.zip && ./aws/install \
    && curl -o aws-iam-authenticator https://amazon-eks.s3.us-west-2.amazonaws.com/1.21.2/2021-07-05/bin/linux/amd64/aws-iam-authenticator \
    && install -o root -g root -m 0755 aws-iam-authenticator /usr/local/bin/aws-iam-authenticator \
    && pip install junit-xml

CMD ["/root/run_custom_tests.sh"]

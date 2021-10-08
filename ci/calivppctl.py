#!/usr/bin/python3

import os
from junit_xml import TestSuite, TestCase

os.system("env KUBECONFIG=/root/kubeconfig calivppctl export")
os.system("mv ./export.tar.gz /root/results")
test_cases = [TestCase('calivppctl', 'calicovpp', 123.345, '/root/results/export.tar.gz', 'No errors....')]
ts = TestSuite("calicovpp test suite", test_cases)

with open('/root/results/junit.xml', 'w') as f:
    TestSuite.to_file(f, [ts], prettyprint=False)

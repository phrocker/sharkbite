import os
import re
import sys
import platform
import subprocess
import multiprocessing

from setuptools import setup, Extension
from setuptools.command.build_ext import build_ext
from distutils.version import LooseVersion

cores = multiprocessing.cpu_count()*1.25

threads="-j" + str(int(cores))



class CMakeExtension(Extension):
    def __init__(self, name, sourcedir=''):
        Extension.__init__(self, name, sources=[])
        self.sourcedir = os.path.abspath(sourcedir)

def readme():
  with open("PYTHONREADME.md", "r") as fh:
    return fh.read()

def operatingsystem():
  if (platform.platform().find("Darwin") >= 0):
    return "Operating System :: MacOS"
  else:   
    return "Operating System :: POSIX :: Linux"

setup(
    name='sharkbite',
    version='1.1.0',
    author='Marc Parisi',
    author_email='phrocker@apache.org',
    url='https://docs.sharkbite.io/',
    description='Apache Accumulo and Apache HDFS Python Connector',
    long_description=readme(),
    long_description_content_type='text/markdown',
    ext_modules=[CMakeExtension('sharkbite.pysharkbite')],
    zip_safe=False,
    classifiers=[
        "Programming Language :: C++",
        "License :: OSI Approved :: Apache Software License",
        operatingsystem(),
    ],
    python_requires='>=3.6',
    packages=['sharkbite']
)

FROM phrocker/hadoop:latest
WORKDIR /
COPY runhdfstests.sh /
COPY hdfswrite.py /
COPY hdfsremove.py /
COPY hdfslist.py /
COPY pysharkbite.cpython-37m-x86_64-linux-gnu.so /
CMD /start-hadoop ; /runhdfstests.sh

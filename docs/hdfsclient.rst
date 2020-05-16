.. image:: https://camo.githubusercontent.com/dbf39cef1a973d8741437693e96b59e31d9e3754/68747470733a2f2f7777772e736861726b626974652e696f2f77702d636f6e74656e742f75706c6f6164732f323031372f30322f736861726b626974652e6a7067

HDFS Client
==================

The hdfs client only supports `mkdir <https://docs.sharkbite.io/en/latest/sharkbitedocs.html#pysharkbite.Hdfs.mkdir>`_ and
`list <https://docs.sharkbite.io/en/latest/sharkbitedocs.html#pysharkbite.Hdfs.list>`_ at this time. Once additional tests are built
around the remainder of the HDFS python functionality we will add in the remaining portions of the client.

An example usage of these functions is below. Note that if Opening RFiles in pysharkbite, you must specify the full path including
the hdfs protocol if it is located on HDFS. This will open a full HDFS client to access these files.

.. code-block:: python

    import pysharkbite

    hdfs = pysharkbite.Hdfs("namenode",8020);

    hdfs.mkdir("/directoryA/directoryB");

    hdfs.list("/");
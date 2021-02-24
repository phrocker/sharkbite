.. image:: https://camo.githubusercontent.com/dbf39cef1a973d8741437693e96b59e31d9e3754/68747470733a2f2f7777772e736861726b626974652e696f2f77702d636f6e74656e742f75706c6f6164732f323031372f30322f736861726b626974652e6a7067

HDFS Client
==================

The `hdfs client  <https://docs.sharkbite.io/en/latest/sharkbitedocs.html#pysharkbite.Hdfs>`_ is nearly full client. It lacks 
features found in the C++ client. These will be added over time. Please visit the link, above, to find the API of what is currently supported

An example usage of these functions is below. Note that if Opening RFiles in pysharkbite, you must specify the full path including
the hdfs protocol if it is located on HDFS. This will open a full HDFS client to access these files.

.. code-block:: python

    import pysharkbite

    hdfs = pysharkbite.Hdfs("hdfs://namenode:8020",8020);

    hdfs.mkdir("/directoryA/directoryB");

    hdfs.list("/");
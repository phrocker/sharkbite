.. image:: https://camo.githubusercontent.com/dbf39cef1a973d8741437693e96b59e31d9e3754/68747470733a2f2f7777772e736861726b626974652e696f2f77702d636f6e74656e742f75706c6f6164732f323031372f30322f736861726b626974652e6a7067

About Sharkbite
==================
**Sharkbite** is an HDFS and native client for key/value stores. With 
initial support for Apache Accumulo Accumulo, the design can and has been used to support other key/value
stores. Development began in 2014 and has slowly evolved. there is no structural specificity to Accumulo
despite being the base implementation. Despite this the examples below will look very much like Accumulo due to aliasing. This is intentional.

Capabilities That will be supported in V1.0 : 

 * Works with Accumulo 1.6.x, 1.7.x, 1.8.x, 1.9.x and 2.x
 * **Read/Write** : Reading and writing data to Accumulo is currently supported.
 * **Table Operations** : Most table operations are currently supported. This includes the fate operations that the normal Accumulo client performs.
 * **Security Operations** : Most security operations are implemented. Please let us know of any gaps.
 
About the name
**************
**Sharkbite's** name originated from design as a connector that abstracted components in which we tightly
coupled and gripped interfaces of the underlying datastore. With an abstraction layer for access, and using
cross compatible objects, the underlying interfaces are heavily coupled to each database. As a result, Sharkbite
became a fitting name since interfaces exist to abstract the high coupling that exists within implementations of 
the API.

Installing
**********
This python client can be installed via `pip install sharkbite`

`A Python example <https://github.com/phrocker/sharkbite/blob/master/examples/pythonexample.py>`_ is included. This is your primary example of the Python bound sharkbite
library.

**Sharkbite** supports async iteration `A simple example <https://github.com/phrocker/sharkbite/blob/master/examples/asyncexample.py>`_ is provided. 

Features
********

HDFS Client
************

**Sharkbite** supports a limited HDFS client. As this functionality grows so will the capabilities. Version 0.7 will support a complete
HDFS client. Since Sharkbite it built as a python bindings around a C++ Client, the python client will mature slightly behind the C++ client,
hence the delta with building this into V 0.7

Version Detection
*****************

**Sharkbite** detects the version of Apache Accumulo. Therefore you will be able to simply create a connector to the zookeeper instance.


Hedged Reads
************

**Sharkbite** supports hedged reads ( executing scans against RFiles when they can be accessed ) concurrently with 
Accumulo RPC scans. The first executor to complete will return your results. This feature is in beta and not suggested
for production environments.

Enable it with the following option:

.. code-block:: python

	import pysharkbite as sharkbite

	connector = sharkbite.AccumuloConnector(user, zk)

  table_operations = connector.tableOps(table)  
	
 	scanner = table_operations.createScanner(auths, 2)
    
  range = sharkbite.Range("myrow")
  
  scanner.addRange( range )
  
  ### enable the beta option of hedged reads
  
  scanner.setOption( sharkbite.ScannerOptions.HedgedReads )
  
  resultset = scanner.getResultSet()
  
  for keyvalue in resultset:
      key = keyvalue.getKey()
      value = keyvalue.getValue()
	


Python Iterators
****************

We now support a beta version of python iterators. By using the cmake option PYTHON_ITERATOR_SUPPORT ( cmake -DPYTHON_ITERATOR_SUPPORT=ON ) we will build the necessary infrastructure to support python iterators

Iterators can be defined as single function lambdas or by implementing the seek or next methods.


The first example implements the seek and onNext methods. seek is optional if you don't wish to adjust the range. Once keys are being iterated you may get the top key. You may call 
iterator.next() after or the infrastructure will do that for you. 

.. code-block:: python

	class myIterator: 
	  def seek(iterator,soughtRange):
	    range = Range("a")
	    iterator.seek(range)
	
	
	  def onNext(iterator):
	    if (iterator.hasTop()):
	    	kv = KeyValue()
	  	  key = iterator.getTopKey()
	  	  cf = key.getColumnFamily()
	  	  value = iterator.getTopValue()
	  	  key.setColumnFamily("oh changed " + cf)
	  	  iterator.next()
	  	  return KeyValue(key,value)
	    else: 
	      return None


If this is defined in a separate file, you may use it with the following code snippet

.. code-block:: python

	with open('test.iter', 'r') as file:
	 iterator = file.read()
	## name, iterator text, priority
	 iterator = pysharkbite.PythonIterator("PythonIterator",iteratortext,100)
	 scanner.addIterator(iterator)    

Alternative you may use lambdas. The lambda you provide will be passed the KeyValue ( getKey() and getValue() return the constituent parts). A partial code example of setting it up is below.
You may return a Key or KeyValue object. If you return the former an empty value will be return ed.

.. code-block:: python

	## define only the name and priority 
	iterator = pysharkbite.PythonIterator("PythonIterator",100)
	## define a lambda to ajust the column family.
	iterator = iterator.onNext("lambda x : Key( x.getKey().getRow(), 'new cf', x.getKey().getColumnQualifier()) ")
	
	scanner.addIterator(iterator)
	
You may either define a python iterator as a text implementation or a lambda. Both cannot be used simulaneously. 

[accumulo]: https://accumulo.apache.org


# ![logo](https://www.sharkbite.io/wp-content/uploads/2017/02/sharkbite.jpg) Sharkbite 
[![Documentation Status](https://readthedocs.org/projects/sharkbite/badge/?version=latest)](https://docs.sharkbite.io/en/latest/?badge=latest)

**S**harkbite is an HDFS and native client for key/value stores. With 
initial support for [Apache Accumulo][accumulo], the design can and has been used to support other key/value
stores. Development began in 2014 and has  slowly evolved. there is no structural specificity to Accumulo
despite being the base implementation. Despite this the examples below will look very much like Accumulo due to aliasing. This is intentional.

Capabilities That will be supported in V0.6 : 

 * Works with Accumulo 1.6.x, 1.7.x, 1.8.x, 1.9.x and 2.x
 * **Read/Write** : Reading and writing data to Accumulo is currently supported.
 * **Table Operations** : Most table operations are currently supported. This includes the fate operations that the normal Accumulo client performs.
 * **Security Operations** : Security operations aren't all implemented, but you should be able to add users, change authorizations, passwords, and remove users.
 
Current Master progress 
  * Development is focusing on abstracting out 2.x changes for Apache Accumulo.
  * Help is desired in abstracting these thrift changes.  

About the name

**S**harkbite's name originated from design as a connector that abstracted components in which we tightly
coupled and gripped interfaces of the underlying datastore. With an abstraction layer for access, and using
cross compatible objects, the underlying interfaces are heavily coupled to each database. As a result, Sharkbite
became a fitting name since interfaces exist to abstract the high coupling that exists within implementations of 
the API.

## Python Support
This python client can be installed via `pip install sharkbite`

[A Python example](https://github.com/phrocker/sharkbite/blob/master/examples/pythonexample.py) is included. This is your primary example of the Python bound sharkbite
library.
## Features


### Hedged Reads (! BETA )

Sharkbite now supports hedged reads ( executing scans against RFiles when they can be accessed ) concurrently with 
Accumulo RPC scans. The first executor to complete will return your results. This feature is in beta and not suggested
for production environments.

Enable it with the following option:

```

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
	
```

### Python Iterators  

We now support a beta version of python iterators. By using the cmake option PYTHON_ITERATOR_SUPPORT ( cmake -DPYTHON_ITERATOR_SUPPORT=ON ) we will build the necessary infrastructure to support python iterators

Iterators can be defined as single function lambdas or by implementing the seek or next methods.


The first example implements the seek and onNext methods. seek is optional if you don't wish to adjust the range. Once keys are being iterated you may get the top key. You may call 
iterator.next() after or the infrastructure will do that for you. 
```

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

```

If this is defined in a separate file, you may use it with the following code snippet

```
with open('test.iter', 'r') as file:
  iterator = file.read()
## name, iterator text, priority
iterator = pysharkbite.PythonIterator("PythonIterator",iteratortext,100)
scanner.addIterator(iterator)    
```

Alternative you may use lambdas. The lambda you provide will be passed the KeyValue ( getKey() and getValue() return the constituent parts). A partial code example of setting it up is below.
You may return a Key or KeyValue object. If you return the former an empty value will be return ed.

```
## define only the name and priority 
iterator = pysharkbite.PythonIterator("PythonIterator",100)
## define a lambda to ajust the column family.
iterator = iterator.onNext("lambda x : Key( x.getKey().getRow(), 'new cf', x.getKey().getColumnQualifier()) ")

scanner.addIterator(iterator)
```

You may either define a python iterator as a text implementation or a lambda. Both cannot be used simulaneously. 

[accumulo]: https://accumulo.apache.org

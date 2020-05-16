
# ![logo](https://www.sharkbite.io/wp-content/uploads/2017/02/sharkbite.jpg) Sharkbite 
![Ubuntu CICD Pipeline](https://github.com/phrocker/sharkbite/workflows/Ubuntu%20CICD%20Pipeline/badge.svg)
[![Build Status](https://travis-ci.org/phrocker/sharkbite.svg?branch=master)](https://travis-ci.org/phrocker/sharkbite)

**S**harkbite is a native client for key/value stores. With 
initial support for [Apache Accumulo][accumulo], the design can and has been used to support other key/value
stores. Development began in 2014 and has  slowly evolved. there is no structural specificity to Accumulo
despite being the base implementation. Despite this the examples below will look very much like Accumulo due to aliasing. This is intentional.

Capabilities That will be supported in V0.6 : 

 * **Pysharkbite**, the native python implementation can be installed via pip install sharkbite for linux and osx
 * Works with Accumulo 1.6.x, 1.7.x, 1.8.x, 1.9.x and 2.x
 * **Read/Write** : Reading and writing data to Accumulo is currently supported.
 * **Table Operations** : Most table operations are currently supported. This includes the fate operations that the normal Accumulo client performs.
 * **Security Operations** : Security operations aren't all implemented, but you should be able to add users, change authorizations, passwords, and remove users.
 * **Python Iterators**  : Python iterators run natively at the tablet server. They are supported either through the labmda iterators in Python or the typical python interfaces. 
 
About the name

**S**harkbite's name originated from design as a connector that abstracted components in which we tightly
coupled and gripped interfaces of the underlying datastore. With an abstraction layer for access, and using
cross compatible objects, the underlying interfaces are heavily coupled to each database. As a result, Sharkbite
became a fitting name since interfaces exist to abstract the high coupling that exists within implementations of 
the API.

## Requirements

	GNU 4+
	cmake
	make
	python3.7-dev ( for python bindings )
	
### For Libthrift
	bison 3+
	libboost.
	automake
	autotool
	autoconf
	libtool
	libevent-dev
	unzip
	
Lib Boost is a compile time dependency for thrift. It is not required at runtime, so will no be referenced via any compiled artifact.
	
### For Native Python Iterators
 	JDK ( such as OpenJDK)
 ## Build Options
 The following is a list of build options in cmake. Preface this with -D at the command line. An example command might be

```
cmake -DPYTHON_ITERATOR_SUPPORT=ON -DCMAKE_BUILD_TYPE=Release -D BUILD_TESTS=ON -D BUILD_PYTHON_TESTS=ON ..
``` 

| CMAKE Option | Default value  |  Purpose  
|--|--|--|
|BUILD_TESTS  | ON | Build all tests
|BUILD_PYTHON_TESTS  | OFF  | Builds python Integration tests that are used for verification and validation
|NATIVE_ARCH  | OFF  | Builds using native architecture symbols. 
|PYTHON_ITERATOR_SUPPORT  | OFF  | Builds the python iterator jar



## Building Linux
```
mkdir build && cd build && cmake .. && cmake --build . ; make test

This will build the package and library, which you can use. It will also build
examples in the examples directory+	
```

## Building on OSX

Follow the same procedures as above, but you may need to force linking bison 3.x
if you installed it via Homebrew

## What we provide

Please note that the library of sharkbite consists of C bindings to allow you to create various connectors
via our C interfaces and a Python binding built via Pybind11. 

## Python Lib
The Python library can be installed by simply typing pip install . into the root source directory.
During this process the C++ library and python bindings will be built.

[A Python example](https://github.com/phrocker/sharkbite/blob/master/examples/pythonexample.py) is included. This is your primary example of the Python bound sharkbite
library.

## Python Iterators (! BETA )

### Recommended
We now support python iterators. If you don't have a need to build sharkbite and don't wish to use JNI for the python bindings, you can build
the native-iterators-jni package without the rest of sharkbite. Simply run mvn package in the native-iterators-jni directory
and the target directory will have the required JAR with all necessary dependencies. 

### Using JNI bindings for python ( not recommended )

By using the cmake option PYTHON_ITERATOR_SUPPORT ( cmake -DPYTHON_ITERATOR_SUPPORT=ON ) we will build the necessary infrastructure to support python iterators using 
either JNI or Jython. 

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

## C Library

The C library is a wrapper around the C++ Code. We supply a target called "capi". We have an example
built to use [python via our C-Library using C-Types ](https://github.com/phrocker/sharkbite/blob/master/c-library-examples/python/example.py].

This python example is vestigial and only exists as an example of the C-API wrappers. Implementations can be written
in Go, Rust, Ruby, etc. 

## Examples

A C++ Example can be found [here](https://www.github.com/phrocker/sharkbite/blob/master/examples/CppExample.cpp) as well as in src/examples/

A Python example can be found [here] (https://github.com/phrocker/sharkbite/blob/master/examples/pythonexample.py)

[Apache Accumulo]: https://accumulo.apache.org

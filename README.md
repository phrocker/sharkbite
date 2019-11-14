# ![logo](https://www.sharkbite.io/wp-content/uploads/2017/02/sharkbite.jpg) Sharkbite 
[![Build Status](https://travis-ci.org/phrocker/sharkbite.svg?branch=master)](https://travis-ci.org/phrocker/sharkbite)

**S**harkbite is a native client for key/value stores. With 
initial support for [Apache Accumulo][accumulo], the design can and has been used to support other key/value
stores. Development began in 2014 and has  slowly evolved. there is no structural specificity to Accumulo
despite being the base implementation. Despite this the examples below will look very much like Accumulo due to aliasing. This is intentional.

Capabilities That will be supported in V0.5 : 

 * Pysharkbite can be installed via pip install pysharkbite for linux and osx
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

## Requirements

	GNU 4+
	cmake
	make
	
### For Libthrift
	bison 3+ 
	automake
	autotool
	autoconf
	libtool
	libevent-dev
	unzip

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

## C Library

The C library is a wrapper around the C++ Code. We supply a target called "capi". We have an example
built to use [python via our C-Library using C-Types ](https://github.com/phrocker/sharkbite/blob/master/c-library-examples/python/example.py].

This python example is vestigial and only exists as an example of the C-API wrappers. Implementations can be written
in Go, Rust, Ruby, etc. 

## Examples

A C++ Example can be found [here](https://www.github.com/phrocker/sharkbite/blob/master/examples/CppExample.cpp) as well as in src/examples/

A Python example can be found [here] (https://github.com/phrocker/sharkbite/blob/master/examples/pythonexample.py)

[accumulo]: https://accumulo.apache.org

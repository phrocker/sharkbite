
***

[![Build Status](https://travis-ci.org/phrocker/sharkbite.svg?branch=master)](https://travis-ci.org/phrocker/sharkbite)


Native KeyValue connector. While initial support has been built for Apache Accumulo, the design
can and has been used to extend to other Key/Value Stores

Currently the code is a merger of C && C++ code. V0.1 will represent a shift more toward 
creating a C API, creating exernalized factories for the thrift code.

Capabilities That will be supported in V0.1  --

  Currently we are known to work with version 1.6.x and 1.7

  --Table Operations--
  Most table operations are currently supported
  This includes the fate operations that the normal Accumulo client performs.
  

  --Security Operations--
  Security operations aren't all implemented, but you should be able to add users, change authorizations, passwords,
  and remove users.


Requirements:

	GNU 4+
	cmake
	make
	thrift
	zookeeper
	libhdfs3

Building
```
	run install scripts located in root directory

	mkdir build && cd build && cmake .. && cmake --build . ; make test

	This will build the package and library, which you can use. It will also build
	examples in the examples directory
```

Creating a Scanner
```
    Configuration conf;
    conf.set("FILE_SYSTEM_ROOT", "/accumulo");
    ZookeeperInstance *instance = new ZookeeperInstance(argv[1], argv[2], 1000,
            &conf);

    AuthInfo creds(argv[3], argv[4], instance->getInstanceId());

    interconnect::MasterConnect *master = new MasterConnect(&creds, instance);

    TableOperations<KeyValue*, ResultBlock<KeyValue*>> *ops = master->tableOps(
            table);
    // create the scanner with ten threads.
     std::unique_ptr<scanners::BatchScanner> scanner = ops->createScanner (&auths, 10);
    // range from a to d
    Key *startkey = new Key();
    startkey->setRow("a", 1);
    Key *stopKey = new Key();
    stopKey->setRow("d", 1);
    Range *range = new Range(startkey, true, stopKey, true); 
    // build your range.
    scanner->addRange(range);

    Results<KeyValue*, ResultBlock<KeyValue*>> *results =
            scanner->getResultSet();

    for (auto iter = results->begin(); iter != results->end(); iter++) {
        KeyValue *kv = *iter;
        if (kv != NULL && kv->getKey() != NULL)
            cout << "got -- " << (*iter)->getKey() << endl;
        else
            cout << "Key is null" << endl;
    }
```


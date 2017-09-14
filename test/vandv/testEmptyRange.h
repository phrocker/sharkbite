/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#ifndef TEST_EMPTY_RANGE
#define TEST_EMPTY_RANGE

#include <iostream>
#include <sstream>

#include "../include/data/constructs/KeyValue.h"
#include "../include/data/constructs/security/Authorizations.h"
#include "../include/scanner/constructs/Results.h"
#include "../include/scanner/impl/Scanner.h"
#include "../include/writer/impl/SinkImpl.h"
#include "../include/data/constructs/client/zookeeperinstance.h"
#include "../include/interconnect/Master.h"
#include "../include/interconnect/tableOps/TableOperations.h"
#include "../include/interconnect/securityOps/SecurityOperations.h"

#include "../include/data/constructs/rfile/RFile.h"
#include "../include/data/constructs/compressor/compressor.h"
#include "../include/data/constructs/compressor/zlibCompressor.h"


#include <assert.h>

#define BOOST_IOSTREAMS_NO_LIB 1


#include "integration.h"

class TestEmptyRange : public IntegrationTest
{
public:
  TestEmptyRange(interconnect::MasterConnect connect) : IntegrationTest(connect)
  {
  }
  std::string testName()
  {
    return "TestEmptyRange";
  }
  int run(std::string table)
  {
    
	std::unique_ptr<interconnect::AccumuloTableOperations> ops = master.tableOps (
	                        table);
	
	std::unique_ptr<interconnect::SecurityOperations> secOps = master.securityOps();

	// create the table. no harm/no foul if it exists
	std::cout << "Checking if " << table << " exists." << std::endl;
	if (!ops->exists ()) {
		std::cout << "Now, creating " << table << std::endl;
		if (!ops->create ()) {
			std::cout << "Could not create table " << std::endl;
		}
		std::this_thread::sleep_for (std::chrono::milliseconds (1000));
	}


	cclient::data::security::Authorizations auths;
	
	cclient::data::security::Authorizations scanAuths;
	scanAuths.addAuthorization("00000001");
	
	if (secOps->createUser("dude","thedude"))
	{
	
	  // now let's drop the user
	
	  secOps->dropUser("dude");
	  
	}
	else
	{
	  std::cout << "Could not create user 'dude'" << std::endl;
	  return -1;
	}
	
	secOps->grantAuthorizations(&scanAuths,"root");

	int fruit_to_write = 200;

	std::cout << "Writing " << fruit_to_write << " apples and bananas" << std::endl;

	std::unique_ptr<writer::Writer> writer = std::unique_ptr<writer::Writer>( dynamic_cast<writer::Writer*>(ops->createWriter (&auths, 15).release()));

	for (int i = 0; i < fruit_to_write; i++) {

		std::shared_ptr<cclient::data::KeyValue> newKv = std::make_shared<cclient::data::KeyValue> ();
		std::shared_ptr<cclient::data::Key> newKey = std::make_shared<  cclient::data::Key> ();
		newKey->setRow ("a", 1);
		newKey->setColFamily ("apple", 5);
		std::stringstream cq;
		cq << "banana" << i;
		newKey->setColQualifier (cq.str ().c_str (), cq.str ().length ());
		newKey->setTimeStamp(1445105294261L);
		newKv->setKey (newKey);
		newKv->setValue (std::make_shared<cclient::data::Value> ());
		writer->push (newKv);
	}
	
	for (int i = 0; i < fruit_to_write; i++) {

	  std::shared_ptr<cclient::data::KeyValue> newKv = std::make_shared<cclient::data::KeyValue> ();
		std::shared_ptr<cclient::data::Key> newKey = std::make_shared<  cclient::data::Key> ();
		newKey->setRow ("a", 1);
		newKey->setColFamily ("avacado", 7);
		std::stringstream cq;
		cq << "banana" << i;
		newKey->setColQualifier (cq.str ().c_str (), cq.str ().length ());
		newKey->setTimeStamp(1445105294261L);
		newKv->setKey (newKey);
		newKv->setValue (std::make_shared< cclient::data::Value> ("fruit"));
		writer->push (newKv);
	}
	
	

	// close will free memory for objects given to it
	writer->close ();

	if ( ops->flush("a","z",true) ) {
		std::cout << "flush successful " << std::endl;
	}


	if (ops->compact("a","z",true)) {
		std::cout << "Compaction successful " << std::endl;
	}
	

      //scan with 10 threads
      int counter = 0;
      try{
	std::cout << "Reading values from row a to d" << std::endl;

	std::unique_ptr<scanners::Scanner> scanner = std::unique_ptr<scanners::Scanner>( dynamic_cast<scanners::Scanner*>(ops->createScanner (&auths, 1).release()));

	// range from a to d
	
	cclient::data::Range *range = new cclient::data::Range ();

	scanner->addRange (std::unique_ptr<cclient::data::Range>(range));
	
	scanner->fetchColumn("avacado");

	scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> *results =
	                scanner->getResultSet ();

	
	
	for (auto iter = results->begin (); iter != results->end ();
	     iter++, counter++) {
		 auto kv =*iter;
		
		  assert(kv->getKey() != NULL);

		
		  std::string st((char*)kv->getValue()->data(),kv->getValue()->size());
		  assert(st == "fruit");
	      
		
	}

	}catch(cclient::exceptions::ClientException ce)
	{
	  std::cout << "Failed message: " << ce.what() << std::endl;
	}


	
	std::cout << "Received " << counter << " results " << std::endl;

	assert(counter >= 200);
	
	ops->remove ();


	std::unique_ptr<interconnect::NamespaceOperations> nameOps = master.namespaceOps();
		
	for(auto nm : nameOps->list())
	{
	  
	  std::cout << "found namespace " << nm << std::endl;
	}
	
	if (nameOps->exists("testing"))
	  nameOps->remove("testing");
	
	if (nameOps->exists("blahblah"))
	  nameOps->remove("blahblah");
	
	nameOps->create("testing");
	
	for(auto nm : nameOps->list())
	{
	  
	  std::cout << "found namespace " << nm << std::endl;
	}
	
	nameOps->rename("blahblah","testing");
	
	
	for(auto nm : nameOps->list())
	{
	  assert(nm != "testing");
	  std::cout << "Found namespace " << nm << std::endl;
	}
	
	assert ( nameOps->remove("blahblah") );
	
	assert(!nameOps->exists());
	
	return 0;
  }
};
#endif

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

#ifndef TEST_DELETE_ALL
#define TEST_DELETE_ALL

#include <iostream>
#include <sstream>

#include "../include/data/constructs/Key.h"
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

#include "../include/data/streaming/HdfsStream.h"

#include <assert.h>

#define BOOST_IOSTREAMS_NO_LIB 1

#include "integration.h"

class TestDeleteAll : public IntegrationTest
{
public:
  TestDeleteAll(interconnect::MasterConnect connect) : IntegrationTest(connect)
  {
  }
  std::string testName()
  {
    return "TestDeleteAll";
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
	else
	  std::cout << table << " exists" << std::endl;


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
	
	if ( ops->flush("","z",true) ) {
		std::cout << "flush successful " << std::endl;
	}
	

	std::unique_ptr<writer::Writer> writer = std::unique_ptr<writer::Writer>( dynamic_cast<writer::Writer*>(ops->createWriter (&auths, 15).release()));

	std::stringstream bigString("ohhi");
	for (int i=0; i < 1*1024; i++)
	{
	  bigString << "blah";
	}
	std::string str = bigString.str();
	
	
	for (int i = 0; i < fruit_to_write; i++) {

		cclient::data::Mutation *newKv = new cclient::data::Mutation ("a");
		
		std::stringstream cq;
		cq << "banana" << i;
		
		newKv->put("avacado",cq.str(),"",1445105294261L);
		writer->addMutation (std::unique_ptr<cclient::data::Mutation>(newKv));
	}
	

	// close will free memory for objects given to it
	writer->close ();
	
	writer = std::unique_ptr<writer::Writer>( dynamic_cast<writer::Writer*>(ops->createWriter (&auths, 15).release()));


	if ( ops->flush("","z",true) ) {
		std::cout << "flush successful " << std::endl;
	}


	if (ops->compact("","",true)) {
		std::cout << "Compaction successful " << std::endl;
	}
	

      //scan with 10 threads
      int counter = 0;
      try{
	std::cout << "Reading values from row a to d" << std::endl;

	std::unique_ptr<scanners::Scanner> scanner = std::unique_ptr<scanners::Scanner>( dynamic_cast<scanners::Scanner*>(ops->createScanner (&auths, 1).release()));

	// range from a to d
	cclient::data::Key *startkey = new cclient::data::Key ();
	startkey->setRow ("a", 1);
	cclient::data::Key *stopKey = new cclient::data::Key ();
	stopKey->setRow ("z", 1);
	cclient::data::Range *range = new cclient::data::Range (startkey, true, stopKey, false);

	scanner->addRange (std::unique_ptr<cclient::data::Range>(range));
	
	scanner->fetchColumn("avacado");

	scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> *results =
	                scanner->getResultSet ();

	writer = std::unique_ptr<writer::Writer>( dynamic_cast<writer::Writer*>(ops->createWriter (&auths, 15).release()));	
	
	for (auto iter = results->begin (); iter != results->end ();
	     iter++, counter++) {
	  
	    
		 std::unique_ptr<cclient::data::KeyValue> kv =*iter;
		
		cclient::data::Mutation *m = new cclient::data::Mutation(kv->getKey()->getRowStr());
		
		std::cout << "Deleting " << kv->getKey() << std::endl;
		
		m->put(kv->getKey()->getColFamilyStr(),kv->getKey()->getColQualifierStr(),kv->getKey()->getColVisibilityStr(),kv->getKey()->getTimeStamp(),true);
		
		writer->addMutation(std::unique_ptr<cclient::data::Mutation>(m));
		
	}

	assert(counter >= 200);
	
	writer->close();
	
	scanner = std::unique_ptr<scanners::Scanner>( dynamic_cast<scanners::Scanner*>(ops->createScanner (&auths, 1).release()));

	range = new cclient::data::Range (startkey, true, stopKey, false);

	scanner->addRange (std::unique_ptr<cclient::data::Range>(range));
	
	scanner->fetchColumn("avacado");

	results =
	                scanner->getResultSet ();	
	//reset the counter
	counter= 0;
	for (auto iter = results->begin (); iter != results->end ();
	     iter++, counter++) {
	  
	    
		 std::unique_ptr<cclient::data::KeyValue> kv =*iter;
		 
		 std::cout << "Still have " << kv->getKey() << std::endl;
		
		
	}
	
	

	
	std::cout << "Received " << counter << " results " << std::endl;

	assert(counter == 0);
	
	if (ops->flush("","z",true)) {
		std::cout << "Compaction successful " << std::endl;
	}
	
	
	if (ops->compact("","",true)) {
		std::cout << "Compaction successful " << std::endl;
	}
	
	writer = std::unique_ptr<writer::Writer>( dynamic_cast<writer::Writer*>(ops->createWriter (&auths, 15).release()));
	
	
	for (int i = 0; i < fruit_to_write; i++) {

		cclient::data::Mutation *newKv = new cclient::data::Mutation ("a");
		
		std::stringstream cq;
		cq << "banana" << i;
		
		newKv->put("avacado",cq.str(),"",1445105294261L);
		writer->addMutation (std::unique_ptr<cclient::data::Mutation>(newKv));
	}
	
	writer->close();
	
	scanner = std::unique_ptr<scanners::Scanner>( dynamic_cast<scanners::Scanner*>(ops->createScanner (&auths, 1).release()));

	range = new cclient::data::Range (startkey, true, stopKey, false);

	scanner->addRange (std::unique_ptr<cclient::data::Range>(range));
	
	scanner->fetchColumn("avacado");

	results =
	                scanner->getResultSet ();	

	for (auto iter = results->begin (); iter != results->end ();
	     iter++, counter++) {
	  
	    
		 std::unique_ptr<cclient::data::KeyValue> kv =*iter;
		
		cclient::data::Mutation *m = new cclient::data::Mutation(kv->getKey()->getRowStr());
		
		std::cout << "Deleting " << kv->getKey() << std::endl;
		
		m->put(kv->getKey()->getColFamilyStr(),kv->getKey()->getColQualifierStr(),kv->getKey()->getColVisibilityStr(),kv->getKey()->getTimeStamp(),true);
		
		writer->addMutation(std::unique_ptr<cclient::data::Mutation>(m));
		
	}

	assert(counter >= 200);
	ops->remove ();
	
	}catch(cclient::exceptions::ClientException ce)
	{
	  std::cout << "Failed message: " << ce.what() << std::endl;
	}



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
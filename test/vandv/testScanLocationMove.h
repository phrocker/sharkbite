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

#ifndef TEST_SCAN_MOVE
#define TEST_SCAN_MOVE

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

#include "../include/data/streaming/HdfsStream.h"

#include <assert.h>

#define BOOST_IOSTREAMS_NO_LIB 1


#include "integration.h"

class TestScansMove : public IntegrationTest
{
public:
  TestScansMove(interconnect::MasterConnect connect) : IntegrationTest(connect)
  {
  }
  std::string testName()
  {
    return "TestScansMove";
  }
  int run(std::string table)
  {
    
	std::unique_ptr<interconnect::AccumuloTableOperations> ops = master.tableOps (
	                        table);
	
	std::unique_ptr<interconnect::SecurityOperations> secOps = master.securityOps();

	// create the table. no harm/no foul if it exists
	std::cout << "Checking if " << table << " exists." << std::endl;
	if (ops->exists())
	  ops->remove();
	else
	  std::cout << "table does not exist" << std::endl;
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

	int fruit_to_write = 2000;

	std::cout << "Writing " << fruit_to_write << " apples and bananas" << std::endl;

	std::unique_ptr<writer::BatchWriter> writer = std::unique_ptr<writer::BatchWriter>( dynamic_cast<writer::BatchWriter*>(ops->createWriter (&auths, 15).release()));

	
	for (int i = 0; i < fruit_to_write; i++) {

		
		
		std::stringstream row;
		row << "a" << i;
		
		cclient::data::Mutation *newKv = new cclient::data::Mutation (row.str());
		
		newKv->put("avacado","banana","00000001",1445105294261L);
		writer->addMutation (std::unique_ptr<cclient::data::Mutation>(newKv));
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

	std::unique_ptr<scanners::Scanner> scanner = std::unique_ptr<scanners::Scanner>( dynamic_cast<scanners::Scanner*>(ops->createScanner (&scanAuths, 1).release()));

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

	
	//ops->addSplits();
	
	for (auto iter = results->begin (); iter != results->end ();
	     iter++, counter++) {
		 std::unique_ptr<cclient::data::KeyValue> kv =*iter;
		
		if (kv != NULL && kv->getKey () != NULL)
			std::cout << "got -- " << kv->getKey () << std::endl;
		else
			std::cout << "Key is null" << std::endl;
		
	}

	std::cout << "Received " << counter << " results " << std::endl;

	assert(counter == 2000);
	scanner = std::unique_ptr<scanners::Scanner>( dynamic_cast<scanners::Scanner*>(ops->createScanner (&auths, 1).release()));
	
	range = new cclient::data::Range (startkey, true, stopKey, false);
	
	scanner->addRange (std::unique_ptr<cclient::data::Range>(range));
	
	scanner->fetchColumn("avacado");

	results =
	                scanner->getResultSet ();

	
	
	for (auto iter = results->begin (); iter != results->end ();
	     iter++, counter++) {
	  
	  if (counter == 500)
	  {
	    std::cout << "Adding splits";
	    std::set<std::string> splits;
	    splits.insert("a10");
	    splits.insert("a20");
	    splits.insert("a50");
	    splits.insert("a60");
	    splits.insert("a7");
	    ops->addSplits(splits);
	  }
		 std::unique_ptr<cclient::data::KeyValue> kv =*iter;
		
		
	}
	
	}catch(cclient::exceptions::ClientException ce)
	{
	  std::cout << "Failed message: " << ce.what() << std::endl;
	}
	


      std::cout << "Received " << counter << " results " << std::endl;

	assert(counter == 2000);
	
	
	
	ops->remove ();


	
	
	return 0;
  }
};
#endif
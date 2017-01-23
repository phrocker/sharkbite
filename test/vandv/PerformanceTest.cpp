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

#ifndef TEST_PERFORMANCE_SUITE
#define TEST_PERFORMANCE_SUITE

#include <iostream>
#include <sstream>
#include <ctime>

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
;

#include "integration.h"

class ReadTest : public IntegrationTest
{
public:
  ReadTest(interconnect::MasterConnect connect) : IntegrationTest(connect)
  {
  }
  std::string testName()
  {
    return "WriteTest";
  }
  int run(std::string table)
  {
    
	std::unique_ptr<interconnect::AccumuloTableOperations> ops = master.tableOps (
	                        table);
	
	std::unique_ptr<interconnect::SecurityOperations> secOps = master.securityOps();

	// create the table. no harm/no foul if it exists
	if (!ops->exists ()) {
		
		if (!ops->create ()) {
			
		}
		std::this_thread::sleep_for (std::chrono::milliseconds (1000));
	}


	clock_t begin = clock();
	clock_t end = clock();
	double elapsed_secs = double(end - begin) / CLOCKS_PER_SEC;
	

	cclient::data::security::Authorizations auths;

      //scan with 10 threads
      int counter = 0;
      try{

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
	begin = clock();
	for (auto iter = results->begin (); iter != results->end ();
	     iter++, counter++) {
	  
	    
		 std::unique_ptr<cclient::data::KeyValue> kv =*iter;
		
		
	}
	
	end = clock();
	elapsed_secs = double(end - begin) / CLOCKS_PER_SEC;
	

	std::cout << "[rate=" << (counter/elapsed_secs) << "]" << std::endl;

	std::cout << "Received " << counter << " results " << std::endl;
	
	assert(counter >= 10000000);
	
	

	
	

	}catch(cclient::exceptions::ClientException ce)
	{
	  std::cout << "Failed message: " << ce.what() << std::endl;
	}

	
	
	return 0;
  }
};

class WriteTest : public IntegrationTest
{
public:
  WriteTest(interconnect::MasterConnect connect) : IntegrationTest(connect)
  {
  }
  std::string testName()
  {
    return "WriteTest";
  }
  int run(std::string table)
  {
    
	std::unique_ptr<interconnect::AccumuloTableOperations> ops = master.tableOps (
	                        table);
	
	std::unique_ptr<interconnect::SecurityOperations> secOps = master.securityOps();

	// create the table. no harm/no foul if it exists
	if (!ops->exists ()) {
		
		if (!ops->create ()) {
			
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
	  return -1;
	}
	
	secOps->grantAuthorizations(&scanAuths,"root");

	int fruit_to_write = 10000000;

	std::cout << "Writing " << fruit_to_write << " apples and bananas" << std::endl;
	
	std::unique_ptr<writer::Writer> writer = std::unique_ptr<writer::Writer>( dynamic_cast<writer::Writer*>(ops->createWriter (&auths, 25).release()));

	std::stringstream bigString("ohhi");
	for (int i=0; i < 1*1024; i++)
	{
	  bigString << "blah";
	}
	std::string str = bigString.str();
	 
	clock_t begin = clock();

  
	try{
  	cclient::data::Mutation *newKv = new cclient::data::Mutation ("a");
	for (int i = 0; i < fruit_to_write; i++) {

		
		
		std::stringstream cq;
		cq << "banana" << i;
		
		newKv->put("avacado",cq.str(),"");
		if ((i%1000) == 0)
		{
		  writer->addMutation (std::unique_ptr<cclient::data::Mutation>(newKv));
		  newKv = new cclient::data::Mutation ("a");
		}
	}
	
	writer->addMutation (std::unique_ptr<cclient::data::Mutation>(newKv));
	

	// close will free memory for objects given to it
	writer->close ();
	clock_t end = clock();
	double elapsed_secs = double(end - begin) / CLOCKS_PER_SEC;
	
	std::cout << "[rate=" << (fruit_to_write/elapsed_secs) << "]" << std::endl;
	

	}catch(cclient::exceptions::ClientException ce)
	{
	  std::cout << "Failed message: " << ce.what() << std::endl;
	}

	
	return 0;
  }
};

int
main (int argc, char **argv)
{

	if (argc < 7) {
		std::cout << "Arguments required: ./performanceTest"
		     << " <instance name> <zks> <user> <password> <table> <type==read||write||drop" << std::endl;
		exit (1);
	}

	std::string table = argv[5];
	std::string type = argv[6];
	


	cclient::data::zookeeper::ZookeeperInstance instance(argv[1],argv[2],1000);

	cclient::data::security::AuthInfo creds (argv[3], argv[4], instance.getInstanceId ());

	interconnect::MasterConnect master(creds,&instance);

	
	
	if (type == "write" )
	{
	  WriteTest test(master);
	  
	  test.run(table);
	}
	else if (type == "drop")
	{
	  std::unique_ptr<interconnect::AccumuloTableOperations> ops = master.tableOps (
	                        table);
	  ops->remove();
	}
	else 
	{
	  ReadTest test(master);
	  
	  test.run(table);
	}
	
	return 0;
}
#endif
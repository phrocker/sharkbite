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

#ifndef TEST_INVALID_SCANS
#define TEST_INVALID_SCANS

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

class TestInvalidScans : public IntegrationTest
{
public:
  TestInvalidScans(interconnect::MasterConnect connect) : IntegrationTest(connect)
  {
  }
  std::string testName()
  {
    return "TestInvalidScans";
  }
  int run(std::string table)
  {
    
	std::unique_ptr<interconnect::AccumuloTableOperations> ops = master.tableOps (
	                        table);
	
	std::unique_ptr<interconnect::SecurityOperations> secOps = master.securityOps();

	// create the table. no harm/no foul if it exists
	std::cout << "Deleting " << table << " if it exists"  << std::endl;
	if (ops->exists ()) {
		ops->remove();
		std::this_thread::sleep_for (std::chrono::milliseconds (1000));
	}
	else
	  std::cout << table << " does not exist" << std::endl;


	cclient::data::security::Authorizations auths;
	

	int fruit_to_write = 200;

	std::cout << "Writing " << fruit_to_write << " apples and bananas" << std::endl;
	
	if ( !ops->flush("","z",true) ) {
		std::cout << "flush not successful as expected " << std::endl;
	}
	
	try{
	std::unique_ptr<writer::BatchWriter> writer = std::unique_ptr<writer::BatchWriter>( dynamic_cast<writer::BatchWriter*>(ops->createWriter (&auths, 15).release()));
	  return -1;
	}
	catch(cclient::exceptions::ClientException &ce)
	{
	  assert(ce.getErrorCode() == TABLE_NOT_FOUND);
	}

	try
	{
	std::unique_ptr<scanners::Scanner> scanner = std::unique_ptr<scanners::Scanner>( dynamic_cast<scanners::Scanner*>(ops->createScanner (&auths, 1).release()));
	return -1;
	}
	catch(cclient::exceptions::ClientException &ce)
	{
	  assert(ce.getErrorCode() == TABLE_NOT_FOUND);
	}
	
	ops->create();
	
	try
	{
	  std::unique_ptr<scanners::Scanner> scanner = std::unique_ptr<scanners::Scanner>( dynamic_cast<scanners::Scanner*>(ops->createScanner (&auths, 1).release()));
	  
	  scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> *results =
			  scanner->getResultSet ();
	  assert(results == NULL);
	  return -1;
	}
	catch(cclient::exceptions::ClientException &ce)
	{
	  assert(ce.getErrorCode() == RANGE_NOT_SPECIFIED);
	}

	ops->remove();
	
	return 0;
  }
};
#endif
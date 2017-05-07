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

#ifndef TEST_INVALID_TOPS
#define TEST_INVALID_TOPS

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


#include <assert.h>

#define BOOST_IOSTREAMS_NO_LIB 1
#include "integration.h"

class TestInvalidTableOps : public IntegrationTest
{
public:
  TestInvalidTableOps(interconnect::MasterConnect connect) : IntegrationTest(connect)
  {
  }
  std::string testName()
  {
    return "TestInvalidTableOps";
  }
  int run(std::string table)
  {
    
	std::unique_ptr<interconnect::AccumuloTableOperations> ops = master.tableOps (
	                        table);


	
	assert(ops->remove() == false);
	try
	{
	  ops = master.tableOps("");
	  return -1;
	}catch(cclient::exceptions::ClientException &ce)
	{
	}
	
	ops = master.tableOps(table);
	if (ops->exists())
	  ops->remove();
	ops->create();
	try{
	  ops->createScanner(0,-1);
	  ops->remove();
	  return -1;
	}catch(cclient::exceptions::ClientException &ce)
	{
	  ops->remove();
	}
	
	std::unique_ptr<interconnect::SecurityOperations> secOps = master.securityOps();
	
	
	assert( secOps->createUser("","") == -1 );
	
	assert( secOps->dropUser("") == -1 );
	
	try
	{
	  secOps->getAuths("");
	  return -1;
	}
	catch(cclient::exceptions::ClientException &ce)
	{
	}
	
	return 0;
  }
};
#endif

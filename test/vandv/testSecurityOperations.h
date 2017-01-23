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

#ifndef TEST_SEC_OPS
#define TEST_SEC_OPS

#include <iostream>
#include <sstream>

#include "../include/data/constructs/KeyValue.h"
#include "../include/data/constructs/security/Authorizations.h"
#include "../include/scanner/constructs/Results.h"
#include "../include/scanner/impl/Scanner.h"
#include "../include/writer/impl/SinkImpl.h"
#include "../include/data/extern/thrift/client_types.h"
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

class TestSecurityOperations : public IntegrationTest
{
public:
  TestSecurityOperations(interconnect::MasterConnect connect) : IntegrationTest(connect)
  {
  }
  std::string testName()
  {
    return "TestSecurityOperations";
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
	
	std::unique_ptr<cclient::data::security::Authorizations> userAuths = secOps->getAuths("root");
	  
	  assert(*userAuths.get() == scanAuths);

	
	try
	{
	secOps->getAuths("user that doesn't exist");
	}
	catch(org::apache::accumulo::core::client::impl::thrift::ThriftSecurityException &tse)
	{
	}
	
	
	
	
	
	
	
	
	ops->remove ();


	
	
	return 0;
  }
};
#endif
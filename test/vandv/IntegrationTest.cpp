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
#include "testWriteRead.h"
#include "bigwrite.h"
#include "deletetest.h"
#include "invalidscans.h"
#include "testAuthsChange.h"
#include "testScanLocationMove.h"
#include "invalidtableops.h"
#include "testSecurityOperations.h"
#include "testEmptyRange.h"
int
main (int argc, char **argv)
{

	if (argc < 5) {
		std::cout << "Arguments required: ./IntegrationTest"
		     << " <instance name> <zks> <user> <password>" << std::endl;
		exit (1);
	}


	std::string table = "IntTestTable";

	cclient::data::zookeeper::ZookeeperInstance instance(argv[1],argv[2],1000);

	cclient::data::security::AuthInfo creds (argv[3], argv[4], instance.getInstanceId ());

	interconnect::MasterConnect master(creds,&instance);

	
	std::vector<IntegrationTest*> tests;
	tests.push_back(new TestWriteRead(master));
	tests.push_back(new TestBigWrite(master));
	tests.push_back(new TestDeleteAll(master));
	tests.push_back(new TestInvalidScans(master));
	tests.push_back(new TestAuthsChange(master));
	tests.push_back(new TestScansMove(master));
	tests.push_back(new TestInvalidTableOps(master));
	tests.push_back(new TestSecurityOperations(master));
	tests.push_back(new TestEmptyRange(master));
	int i=0; 
	for(auto test : tests)
	{
	  std::cout << "Running " << test->testName() << "..." << std::endl;
	std::stringstream ss;
	ss << table << i;
	  assert( test->run(ss.str()) == 0);
	  std::cout << "Finished " << test->testName() << std::endl;
	  i++;

	}

	return 0;
}


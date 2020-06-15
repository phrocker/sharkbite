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
#include <assert.h>

#include "../include/data/constructs/KeyValue.h"
#include "../include/data/constructs/security/Authorizations.h"
#include "../include/scanner/constructs/Results.h"
#include "../include/scanner/impl/Scanner.h"
#include "../include/data/constructs/client/zookeeperinstance.h"
#include "../include/interconnect/Accumulo.h"
#include "../include/interconnect/tableOps/TableOperations.h"

#include "../include/data/constructs/rfile/RFile.h"
#include "../include/data/constructs/compressor/compressor.h"
#include "../include/data/constructs/compressor/zlibCompressor.h"


#define BOOST_IOSTREAMS_NO_LIB 1


bool
keyCompare (std::shared_ptr<cclient::data::KeyValue>  a, std::shared_ptr<cclient::data::KeyValue>  b)
{
	return *(a->getKey ()) < *(b->getKey ());
}

int
main (int argc, char **argv)
{


	if (argc < 6) {
		std::cout << "Arguments required: ./TableOps"
		     << " <instance name> <zks> <user> <password>"
		     << " <table>" << std::endl;
		exit (1);
	}

	std::string table = argv[5];

	cclient::impl::Configuration *confC = (new cclient::impl::Configuration());
	confC->set ("FILE_SYSTEM_ROOT", "/accumulo");
	
	auto instance = std::make_shared< cclient::data::zookeeper::ZookeeperInstance >(argv[1], argv[2], 1000,
	                std::unique_ptr<cclient::impl::Configuration>(confC));

	cclient::data::security::AuthInfo creds (argv[3], argv[4], instance->getInstanceId ());

	interconnect::AccumuloConnector accumulo(&creds, instance);

	std::unique_ptr<interconnect::TableOperations<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>> ops = accumulo.tableOps (
	                        table);

	// create the table. no harm/no foul if it exists
	std::cout << "Checking if " << table << " exists." << std::endl;
	if (!ops->exists ()) {
		std::cout << "It does, so I am creating it." << std::endl;
		ops->create ();
	} else {
		std::cout << table << " already exists, not creating it" << std::endl;
	}


	std::cout << "Compacting table " << std::endl;
	ops->compact("a","z",false);

	std::unique_ptr<interconnect::NamespaceOperations> nameOps = accumulo.namespaceOps();
		
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
	}
	
	nameOps->remove("blahblah");
	
	assert(!nameOps->exists());


	return 0;
}


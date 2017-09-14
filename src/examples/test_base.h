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
#include <algorithm>
#include <sstream>

#include "data/constructs/KeyValue.h"
#include "data/constructs/security/Authorizations.h"
#include "scanner/constructs/Results.h"
#include "scanner/impl/Scanner.h"
#include "writer/impl/SinkImpl.h"
#include "data/constructs/client/zookeeperinstance.h"
#include "interconnect/Master.h"
#include "interconnect/tableOps/TableOperations.h"
#include "interconnect/securityOps/SecurityOperations.h"

#include "data/constructs/rfile/RFile.h"
#include "data/constructs/compressor/compressor.h"
#include "data/constructs/compressor/zlibCompressor.h"


#include <assert.h>

#define BOOST_IOSTREAMS_NO_LIB 1

#ifndef INSERT_TEST_H
#define INSERT_TEST_H 1
int run_test(std::string table, std::string instanceStr, std::string zks, std::string username, std::string password, uint64_t numToWrite)
{
	cclient::data::zookeeper::ZookeeperInstance *instance = 0;
	
	try{
	cclient::impl::Configuration *confC = (new cclient::impl::Configuration());
	confC->set ("FILE_SYSTEM_ROOT", "/accumulo");
	
	instance = new cclient::data::zookeeper::ZookeeperInstance (instanceStr,zks, 1000,
	                std::unique_ptr<cclient::impl::Configuration>(confC));
	}catch(cclient::exceptions::ClientException ce)
	{
	  std::cout << "Could not connect to ZK. Error: " << ce.what()  << std::endl;
	  return 1;
	}

	cclient::data::security::AuthInfo creds (username, password, instance->getInstanceId ());

	interconnect::MasterConnect *master = 0;
	
	try
	{
	  master = new interconnect::MasterConnect (&creds, instance);
	
	}catch(cclient::exceptions::ClientException ce)
	{
	  std::cout << "Could not connect to Master. Error: " << ce.what()  << std::endl;
	  return 1;
	}

	
	std::unique_ptr<interconnect::AccumuloTableOperations> ops = master->tableOps (
	                        table);
	

	
	std::unique_ptr<interconnect::SecurityOperations> secOps = master->securityOps();

	// create the table. no harm/no foul if it exists
	std::cout << "Checking if " << table << " exists." << std::endl;
	if (!ops->exists ()) {
		std::cout << "Now, creating " << table << std::endl;
		if (!ops->create ()) {
			std::cout << "Could not create table " << std::endl;
		}
		std::this_thread::sleep_for (std::chrono::milliseconds (1000));
	}
	
	std::set<std::string> tables = ops->listTables();
	

	auto it = find (tables.begin(), tables.end(), table);	
	
	if (it == tables.end())
	  throw std::runtime_error("Could not find table");
	
	for(auto table : tables)
	{
	  std::cout << "Table : " << table << std::endl;
	}
	
	cclient::data::security::Authorizations auths;

	std::cout << "Writing " << numToWrite << " apples and bananas" << std::endl;

	std::unique_ptr<writer::Writer> sink = std::unique_ptr<writer::Writer>( dynamic_cast<writer::Writer*>(ops->createWriter (&auths, 15).release()));

	std::map<std::string,std::string> tableOps = ops->getProperties();
	
	if (tableOps["table.split.threshold"] != "1G")
	{
	    std::cout << "Unknown default table configuration!!!" << std::endl;
	}
	
	ops->setProperty("table.split.threshold","1G");
	
	tableOps = ops->getProperties();
	
	if (tableOps["table.split.threshold"] != "1G")
	{
	    std::cout << "Could not set table configuration" << std::endl;
	    ops->remove();
	    return 1;
	}
	else
	{
	    std::cout << "Successfully set table.split.threshold to 1K " << std::endl;
	}
	
	try{
	  for (int i = 0; i < numToWrite; i++) {

		  std::shared_ptr<cclient::data::KeyValue> newKv = std::make_shared<cclient::data::KeyValue> ();
		  std::shared_ptr<cclient::data::Key> newKey = std::make_shared<cclient::data::Key> ();
		  std::stringstream row;
		  row << "a" << i;
		  newKey->setRow (row.str());
		  newKey->setColFamily ("apple", 5);
		  std::stringstream cq;
		  cq << "banana" << i;
		  newKey->setColQualifier (cq.str ().c_str (), cq.str ().length ());
		  newKey->setTimeStamp(1445105294261L);
		  newKv->setKey (newKey,true);
		  std::shared_ptr<cclient::data::Value> v = std::make_shared<cclient::data::Value>();
		  newKv->setValue (v);
		  sink->push (newKv);
	  }
	}catch(cclient::exceptions::ClientException ce)
	{
	  std::cout << "Exception is  " << ce.what() << std::endl;
	}

	// close will free memory for objects given to it
	sink->close ();
	std::cout << "initiate flush " << std::endl;

	if ( ops->flush("a","z",true) ) {
		std::cout << "flush successful " << std::endl;
	}
	else
		std::cout << "flush unsuccessful" << std::endl;

	ops->compact("a","z",true);
	
	std::set<std::string> shplits;
	shplits.insert("apple");
	shplits.insert("banana");
	
	ops->addSplits(shplits);
	
	std::vector<std::string> splits = ops->listSplits();
	
	for(std::string split : splits)
	{
	    std::cout << "split " << split << std::endl;
	}
	
	
	std::cout << "Removing table" << std::endl;
	
	//ops->remove();
	
	tables = ops->listTables();
	

	it = find (tables.begin(), tables.end(), table);	
	
	
	for(auto table : tables)
	{
	  std::cout << "Table : " << table << std::endl;
	}


	delete master;
	
	delete instance;

	return 0;
}

#endif


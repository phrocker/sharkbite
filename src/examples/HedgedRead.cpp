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
 * See the License for the specific language governing premissions and
 * limitations under the License.
 */

#include <iostream>
#include <sstream>
#include <fstream>

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
#include "scanner/ScannerOptions.h"


#include <assert.h>


int
main (int argc, char **argv)
{

	
if (argc < 2) {
    std::cout << "Arguments required: ./HedgedRead" << " -t table -r <row> -i <instance name> -z <zookeeper list> -u <username> -p <password>  " << std::endl;
    std::cout << "Optional arguments:     -v <visibility> -- visibility  " << std::endl;
    exit(1);
  }

  logging::LoggerConfiguration::enableTraceLogger();


  std::string row,instancename,zookeepers,user,password,table;
  std::string visibility;

  if (argc >= 2) {
    for (int i = 1; i < argc; i++) {
      // always assume big endian
      std::string key = argv[i];
    

      if (key == "-v") {
        if (i + 1 < argc) {
          visibility = argv[i + 1];
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply visibility");
        }
      }

	   if (key == "-u") {
        if (i + 1 < argc) {
          user = argv[i + 1];
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply username");
        }
      }

	   if (key == "-p") {
        if (i + 1 < argc) {
          password = argv[i + 1];
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply password");
        }
      }

	   if (key == "-i") {
        if (i + 1 < argc) {
          instancename = argv[i + 1];
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply instancename");
        }
      }

	   if (key == "-z") {
        if (i + 1 < argc) {
          zookeepers = argv[i + 1];
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply zookeepers");
        }
      }

	   if (key == "-t") {
        if (i + 1 < argc) {
          table = argv[i + 1];
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply table name");
        }
      }

      if (key == "-r") {
        if (i + 1 < argc) {
          row = argv[i + 1];
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply row");
        }
      }

    }
  
  }
	
	
	std::shared_ptr<cclient::data::zookeeper::ZookeeperInstance> instance = 0;
	std::shared_ptr<cclient::impl::Configuration> confC = std::make_shared<cclient::impl::Configuration>();
		
	confC->set ("FILE_SYSTEM_ROOT", "/accumulo");
		
	instance = std::make_shared< cclient::data::zookeeper::ZookeeperInstance >(instancename,zookeepers, 1000,confC);
	

	cclient::data::security::AuthInfo creds (user,password, instance->getInstanceId ());

	interconnect::MasterConnect *master = 0;
	
	try{
	master = new interconnect::MasterConnect (creds, instance);
	}catch(const cclient::exceptions::ClientException &ce)
	{
	  std::cout << "Could not connect: " << ce.what() << std::endl;
	  return 1;
	}

	try{
		auto ops = master->tableOps (
								table);

		cclient::data::security::Authorizations scanAuths;


		auto scanner = ops->createScanner (&scanAuths, 1);

		scanner->addRange (std::make_unique<cclient::data::Range>(row));

		scanner->setOption(ScannerOptions::ENABLE_HEDGED_READS);

		auto results =
						scanner->getResultSet ();

		for(const auto &kv : *results){
			std::cout << kv->getKey() << std::endl;
		}
		

	}catch(const cclient::exceptions::ClientException &ce)
	{
	  std::cout << "Failed message: " << ce.what() << std::endl;
	}

	delete master;

	return 0;
}


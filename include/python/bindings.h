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
#include <pybind11/pybind11.h>
#include <pybind11/functional.h>


#include "data/constructs/KeyValue.h"
#include "data/constructs/security/Authorizations.h"
#include "scanner/constructs/Results.h"
#include "scanner/impl/Scanner.h"
#include "data/constructs/client/zookeeperinstance.h"
#include "interconnect/Master.h"
#include "interconnect/tableOps/TableOperations.h"

#include "data/constructs/rfile/RFile.h"
#include "data/constructs/compressor/compressor.h"
#include "data/constructs/compressor/zlibCompressor.h"



/*
 * 	cclient::impl::Configuration *confC = (new cclient::impl::Configuration());
	confC->set ("FILE_SYSTEM_ROOT", "/accumulo");

	cclient::data::zookeeper::ZookeeperInstance *instance = new cclient::data::zookeeper::ZookeeperInstance (argv[1], argv[2], 1000,
	                std::unique_ptr<cclient::impl::Configuration>(confC));

	cclient::data::security::AuthInfo creds (argv[3], argv[4], instance->getInstanceId ());

	interconnect::MasterConnect master(&creds, instance);

	std::unique_ptr<interconnect::TableOperations<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>> ops = master.tableOps (
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
	}

	nameOps->remove("blahblah");

	assert(!nameOps->exists());

 *
 *
 */

PYBIND11_MODULE(sharkbite, s){
	s.doc() = "Sharkbite connector plugin";

	pybind11::class_<cclient::impl::Configuration>(s, "Configuration")
	   .def(pybind11::init([]() { // Note: no `self` argument
	        return new cclient::impl::Configuration();// return by raw pointer
	        // or: return std::make_unique<Foo>(...); // return by holder
	        // or: return Foo(...); // return by value (move constructor)
	    }));

}

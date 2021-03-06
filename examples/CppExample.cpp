#include <iostream>
#include <sstream>
#include <fstream>

#include "data/constructs/Key.h"
#include "data/constructs/KeyValue.h"
#include "data/constructs/security/AuthInfo.h"
#include "data/constructs/security/Authorizations.h"
#include "scanner/constructs/Results.h"
#include "scanner/impl/Scanner.h"
#include "writer/impl/SinkImpl.h"
#include "data/constructs/client/zookeeperinstance.h"
#include "interconnect/Accumulo.h"
#include "interconnect/tableOps/TableOperations.h"
#include "interconnect/securityOps/SecurityOperations.h"

int main(int argc, char **argv) {

//This code shows an example of reading data from an Accumulo instance.

	std::string instancestr = argv[1];

	std::string zookeepers = argv[2];

	std::string username = argv[3];

	std::string password = argv[4];

	std::string table = "testtable";

	auto instance = new cclient::data::zookeeper::ZookeeperInstance(instancestr,
			zookeepers, 1000);

	cclient::data::security::AuthInfo creds(username, password,
			instance->getInstanceId());

	auto coordinator = new interconnect::AccumuloConnector(&creds, instance);

	auto ops = coordinator->tableOps(table);

	// create the scanner with ten threads.

	cclient::data::security::Authorizations auths;

	auto scanner = ops->createScanner(&auths, 10);

	// range from a to d

	auto startkey = std::make_shared<cclient::data::Key>();

	startkey->setRow("a");

	auto stopKey = std::make_shared<cclient::data::Key>();

	stopKey->setRow("d");

	// use keys or strings
	cclient::data::Range range(sartKey, true, stopKey, true);
	// ** OR **
	// cclient::data::Range range("a", true, "d", true);

	// add your range.

	scanner->addRange(range);

	auto results = scanner->getResultSet();

	for (const auto &iter : results) {

		auto kv = *iter;

		if (kv != NULL && kv->getKey() != NULL)

			cout << "got -- " << (*iter)->getKey() << endl;

		else

			cout << "Key is null" << endl;

	}
}

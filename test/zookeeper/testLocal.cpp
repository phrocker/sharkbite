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
#include <string>
#include <sstream>
#include <assert.h>
using namespace std;

#include "../../include/data/constructs/client/zookeeper/zoocache.h"

#include "../../include/data/constructs/client/zookeeper/zookeepers.h"
using namespace cclient::data::zookeeper;


/**
 * Assumes smac is running
 **/
int main(int argc, char **argv) {

	string host = "localhost:2183";
	string instance = "smac";
	int timeout = 1000;
	if (argc == 3) {
		host = argv[1];
		instance = argv[2];
	}
	Watch myWatch;
	ZooKeeper *newKeeper = new ZooKeeper(host.c_str(), timeout);
	newKeeper->init(&myWatch);

	ZooCache cache(newKeeper);
 
	stringstream instancePath;

	instancePath << ZROOT << ZINSTANCES << "/" << instance;

	uint8_t *instanceId = cache.getData(instancePath.str());

	stringstream zRootLocation;

	zRootLocation << ZROOT << "/" << instanceId << "/" << "tservers";

	cout << "looking for tservers at " << zRootLocation.str() << endl;

	vector<string> children = cache.getChildren(zRootLocation.str());
	
	assert(children.size() == 3 );
	
	cout << "Found all three instances from SMAC" << endl;

	delete newKeeper;
	return 0;
}


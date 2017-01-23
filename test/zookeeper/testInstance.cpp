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
using namespace std;

#include "../../include/data/constructs/client/zookeeper/zoocache.h"

#include "../../include/data/constructs/client/zookeeper/zookeepers.h"
#include "../../include/data/constructs/client/zookeeperinstance.h"
using namespace cclient::data::zookeeper;
#include <assert.h>

int main(int argc, char **argv) {


	string host = "localhost:2183";
	string instance = "smac";
	if (argc == 3) {
		host = argv[1];
		instance = argv[2];
	}
	ZookeeperInstance zkInst(instance,host,1000);

	cout << zkInst.getInstanceId() << endl;

	vector<string> locs = zkInst.getMasterLocations();
	
	assert(locs.size() == 1);
	
	assert(zkInst.getInstanceId().length() > 0);
	
	assert( zkInst.getInstanceName() == "smac" );


	return 0;
}


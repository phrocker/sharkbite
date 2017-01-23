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
#include "../../../../include/data/client/test/../RootTabletLocator.h"
#include "../../../../include/data/client/test/../MetaDataLocationObtainer.h"
#include "../../../../include/data/client/test/../TabletServerLocator.h"
#include "../../../../include/data/client/test/../ExtentLocator.h"
#include "../../../../include/data/client/test/../LocatorCache.h"
#include "../../../../include/data/client/test/../../constructs/client/zookeeperinstance.h"
using namespace cclient::impl;
int main(int argc, char **argv) {

	string host = "127.0.0.1:2181";
	string instance = "instance";
	uint32_t timeout = 1000;
	if (argc == 3) {
		host = argv[1];
		instance = argv[2];
	}
	ZookeeperInstance *zkInst = new ZookeeperInstance(instance, host, timeout,
			new Configuration());

	//RootTabletLocator *locator = new RootTabletLocator(zkInst);

	cout << 	"root tablet location " << zkInst->getRootTabletLocation() << endl;;

	AuthInfo creds("root","secret",zkInst->getInstanceId());

	// blah table
	TabletLocator *locator = cachedLocators.getLocator(LocatorKey(zkInst,"2"));

	locator->locateTablet(&creds,"a",false,true);
	vector<Range*> ranges;

	Key startKey;
	startKey.setRow("a",1);
	Key stopKey;
	stopKey.setRow("b",1);
	Range range(&startKey,true,&stopKey,true);
	ranges.push_back(&range);
	map<string, map<KeyExtent*, vector<Range*>, pointer_comparator<KeyExtent*> > > binnedRanges;
	locator->binRanges(&creds,ranges,&binnedRanges);
	for(auto host : binnedRanges)
	{
		cout << "Host : " << host.first << endl;
		for(auto extentToRange : host.second)
		{
			cout << extentToRange.first->getTableId() << " " << extentToRange.first->getEndRow() << endl;
			cout << extentToRange.first->getTableId() << " " << extentToRange.first->getPrevEndRow() << endl;
			for(auto ranges : extentToRange.second)
			{
				cout << " start -- " << *(ranges->getStartKey()) << " end -- " << (ranges->getStopKey()) << endl;
			}
		}
	}
/*
	MetaDataLocationObtainer *metaObtainer = new MetaDataLocationObtainer(&creds,zkInst);

	cout << "ha" << endl;
	KeyExtent testExtent("!0","!0<","");
	cout << "ha2"<< endl;
	//TabletLocation testLocation(ROOT_EXTENT,zkInst->getRootTabletLocation(),"");
	TabletLocation testLocation(ROOT_EXTENT,zkInst->getRootTabletLocation(),"");

	cout << "find"<< endl;
	//metaObtainer->findTablet(&testLocation,"!","<",locator);
	metaObtainer->findTablet(&testLocation,"!0;1;","!0<",locator);
	//metaObtainer->findTablet(&testLocation,"!","<",locator);
*/



	delete locator;
//	delete metaObtainer;
	delete zkInst;



	return 0;
}



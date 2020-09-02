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
#include "data/constructs/client/./zookeeperinstance.h"

#include <algorithm>
#include <sstream>
#include <string>
#include <vector>

#include "data/constructs/client/../../../interconnect/InterConnect.h"
#include "data/constructs/client/../configuration/Configuration.h"
#include "data/constructs/client/../inputvalidation.h"
#include "data/constructs/client/./zookeeper/zoocache.h"
#include "data/constructs/client/./zookeeper/zookeepers.h"
#include "data/constructs/client/Instance.h"
#include "data/exceptions/ClientException.h"

namespace cclient {
namespace data {
namespace zookeeper {

std::string ZookeeperInstance::getRootTabletLocation() {
  std::stringstream zRootLocation;

  zRootLocation << getRoot() << ZROOT_TABLET_LOCATION;

  uint8_t *rootLoc = myZooCache->getData(zRootLocation.str());

  if (IsEmpty((char *)rootLoc)) {
    return NULL;
  }

  return split(std::string((char *)rootLoc), '|').at(0);
}

std::string ZookeeperInstance::getZookeepers() const { return zookeeperList; }

std::string ZookeeperInstance::getRoot() {
  std::stringstream st;
  st << ZROOT << "/" << instanceId;
  return st.str();
}

std::vector<std::string> ZookeeperInstance::getMasterLocations() {
  std::string masterLockPath = getRoot() + ZMASTER_LOCK;

  std::vector<std::string> lockData = myZooCache->getChildren(masterLockPath);

  std::vector<std::string> masters;

  if (IsEmpty(&lockData)) {
    return masters;
  }

  masters.insert(masters.begin(), lockData.begin(), lockData.end());

  std::sort(masters.begin(), masters.end());

  std::stringstream ss;
  ss << masterLockPath << "/" << masters.at(0);

  std::vector<std::string> firstMaster;

  std::string str = std::string((const char *)myZooCache->getData(ss.str()));
  firstMaster.push_back(str);

  return firstMaster;
}
//
std::string ZookeeperInstance::getInstanceId(bool retry) {
  if (IsEmpty(&instanceId)) {
    std::stringstream instancePath;

    instancePath << ZROOT << ZINSTANCES << "/" << instanceName;

    uint8_t *zkInstanceId = myZooCache->getData(instancePath.str());

    if (IsEmpty(zkInstanceId)) {
      if (!retry)
        throw cclient::exceptions::ClientException(
            "Instance Id does not exist within zookeeper at path  " +
            instancePath.str());
      else
        return "";
    }

    instanceId.insert(0, (char *)zkInstanceId);
  }
  return instanceId;
}

std::vector<std::shared_ptr<interconnect::ServerConnection>>
ZookeeperInstance::getServers() {
  std::string tserverPath = getRoot() + TSERVERS;

  std::vector<std::string> servers = myZooCache->getChildren(tserverPath);

  std::vector<std::shared_ptr<interconnect::ServerConnection>> tabletServers;

  if (IsEmpty(&servers)) {
    return tabletServers;
  }

  for (auto server : servers) {
    std::stringstream ss;
    ss << tserverPath << "/" << server;
    std::string data = std::string((const char *)myZooCache->getData(ss.str()));

    std::vector<std::string> addresses = split(data, ';');
    if (addresses.size() == 0 && IsEmpty(&data)) {
      addresses.push_back(server);
    } else if (addresses.size() == 0) {
      addresses.push_back(data);
    }
    for (auto address : addresses) {
      std::vector<std::string> addressSplit = split(address, '=');
      std::string location = "";
      if (addressSplit.size() != 2) {
        if (addressSplit.size() == 0)
          location = address;
        else
          location = addressSplit.at(0);
      } else {
        location = addressSplit.at(1);
      }

      std::vector<std::string> locationAndPort = split(location, ':');
      if (locationAndPort.size() != 2) {
        throw cclient::exceptions::ClientException(INVALID_ZK_SERVER_DATA);
      }
      char *res = 0;
      errno = 0;
      int64_t port = strtol(locationAndPort.at(1).c_str(), &res, 10);
      if (((port == LONG_MIN || port == LONG_MAX) && errno != 0) ||
          *res != '\0') {
        throw cclient::exceptions::ClientException(INVALID_ZK_SERVER_PORT);
      }

      std::shared_ptr<interconnect::ServerConnection> newConnection =
          std::make_shared<interconnect::ServerConnection>(
              locationAndPort.at(0), port, -1);
      tabletServers.push_back(newConnection);
    }
  }

  return tabletServers;
}

std::string ZookeeperInstance::getInstanceName() const { return instanceName; }

const cclient::impl::Configuration *ZookeeperInstance::getConfiguration() {
  return myConfiguration.get();
}
/*
 void
 ZookeeperInstance::setConfiguration(std::unique_ptr<cclient::impl::Configuration>
 conf) { myConfiguration = std::move(conf);
 }
 */
}  // namespace zookeeper
}  // namespace data
}  // namespace cclient

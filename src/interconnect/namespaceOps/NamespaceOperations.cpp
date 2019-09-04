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

#include <sstream>
#include <string>
#include <mutex>

#include "data/exceptions/ClientException.h"
#include "interconnect/namespaceOps/NamespaceOperations.h"
#include "interconnect/tableOps/../../data/constructs/ConfigurationOptions.h"

namespace interconnect {

NamespaceOperations::~NamespaceOperations() {

}

bool NamespaceOperations::exists(std::string name) {
  if (IsEmpty(&name))
    name = myNamespace;
  return namespaces.find(name) != std::end(namespaces);
}

void NamespaceOperations::create(std::string name) {
  interconnect::AccumuloMasterTransporter *baseTransport = clientInterface->getTransport().get();

  if (IsEmpty(&name))
    name = myNamespace;
  if (!baseTransport->createNamespace(credentials, name)) {
    throw cclient::exceptions::ClientException(COULD_NOT_CREATE_NAMESPACE);
  } else {
    loadNamespaces();
  }
}

bool NamespaceOperations::remove(std::string name) {
  if (IsEmpty(&name))
    name = myNamespace;
  std::string id = namespaces[name];

  if (IsEmpty(&id) || id == "accumulo") {
    throw cclient::exceptions::ClientException(DELETE_DEFAULT_NAMESPACE);
  } else {
    interconnect::AccumuloMasterTransporter *baseTransport = clientInterface->getTransport().get();

    if (!baseTransport->deletenamespace(credentials, name)) {
      throw cclient::exceptions::ClientException(COULD_NOT_CREATE_NAMESPACE);
    } else {
      loadNamespaces();
      return true;
    }
  }

}

void NamespaceOperations::rename(std::string newName, std::string oldName) {
  if (IsEmpty(&oldName)) {
    oldName = myNamespace;
  }

  std::string id = namespaces[oldName];

  if (IsEmpty(&id) || id == "accumulo") {
    throw cclient::exceptions::ClientException(DELETE_DEFAULT_NAMESPACE);
  } else {
    interconnect::AccumuloMasterTransporter *baseTransport = clientInterface->getTransport().get();

    if (!baseTransport->renamenamespace(credentials, oldName, newName)) {
      throw cclient::exceptions::ClientException(COULD_NOT_CREATE_NAMESPACE);
    } else {
      loadNamespaces();
    }
  }
}

std::string NamespaceOperations::systemNamespace() {
  return "accumulo";
}

std::vector<std::string> NamespaceOperations::list() {

  return namespaceNames;
}

void NamespaceOperations::loadNamespaces(bool force) {
  std::lock_guard<std::recursive_mutex> lock(namesOpMutex);
  const cclient::impl::Configuration *conf = myInstance->getConfiguration();

  std::string fsRoot = conf->get(FILE_SYSTEM_ROOT_CFG);

  fsRoot.append("/");
  fsRoot.append(myInstance->getInstanceId());
  fsRoot.append( TABLE_GET_NAMESPACES);

  cclient::data::InstanceCache *cache = myInstance->getInstanceCache();
  std::vector<std::string> namespaceIds = cache->getChildren(fsRoot, force);

  namespaces.clear();
  namespaceNames.clear();
  for (std::string retrievedId : namespaceIds) {
    std::string tablePath = fsRoot;
    tablePath.append("/");
    tablePath.append(retrievedId);

    std::string namePath = tablePath;
    namePath.append(TABLE_GET_NAME);
    char *path = (char*) cache->getData(namePath);
    if (IsEmpty(path)) {

      continue;

    }
    std::string namespaceName = std::string(path);

    if (!IsEmpty(&namespaceName)) {
      // insert both representations
      namespaces.insert(std::make_pair(retrievedId, namespaceName));
      namespaces.insert(std::make_pair(namespaceName, retrievedId));
      namespaceNames.push_back(namespaceName);
    }

  }

}

std::map<std::string, std::string> NamespaceOperations::getProperties(std::string namespaceName) {
  std::string nm = namespaceName;
  if (IsEmpty(&namespaceName)) {
    nm = myNamespace;
  }
  interconnect::AccumuloMasterTransporter *baseTransport = clientInterface->getTransport().get();
  return baseTransport->getNamespaceConfiguration(credentials, nm);
}

void NamespaceOperations::removeProperty(std::string property, std::string namespaceName) {
  std::string nm = namespaceName;
  if (IsEmpty(&namespaceName)) {
    nm = myNamespace;
  }
  interconnect::AccumuloMasterTransporter *baseTransport = clientInterface->getTransport().get();
  baseTransport->removeNamespaceProperty(credentials, nm, property);
}
void NamespaceOperations::setProperty(std::string property, std::string value, std::string namespaceName) {
  std::string nm = namespaceName;
  if (IsEmpty(&namespaceName)) {
    nm = myNamespace;
  }
  interconnect::AccumuloMasterTransporter *baseTransport = clientInterface->getTransport().get();
  baseTransport->setNamespaceProperty(credentials, nm, property, value);
}

void NamespaceOperations::attachIterator(cclient::data::IterInfo setting, cclient::data::ITERATOR_TYPES scope, std::string namespaceName) {
  std::string nm = namespaceName;
  if (IsEmpty(&namespaceName)) {
    nm = myNamespace;
  }
  for (cclient::data::ITERATOR_TYPES cType : cclient::data::TypeIterator()) {
    if ((scope | cType) != cType) {
      continue;
    }
    std::stringstream root;
    root << "table.iterator" << cType << "." << setting.getName();
    for (auto prop : setting.getOptions()) {
      std::stringstream property(root.str());
      property << ".opt" << prop.first;
      setProperty(property.str(), prop.second, nm);
    }
    std::stringstream rootValue;
    rootValue << setting.getPriority() << "," << setting.getClass();
    setProperty(root.str(), rootValue.str(), nm);
  }
}

void NamespaceOperations::removeIterator(std::string name, cclient::data::ITERATOR_TYPES scope, std::string namespaceName) {
  std::string nm = namespaceName;
  if (IsEmpty(&namespaceName)) {
    nm = myNamespace;
  }
  std::map<std::string, std::string> props = getProperties();
  for (cclient::data::ITERATOR_TYPES cType : cclient::data::TypeIterator()) {
    if ((scope | cType) != cType) {
      continue;
    }
    std::stringstream root;
    root << "table.iterator" << cType << "." << name;
    std::string rootStr = root.str();
    root << ".opt";
    std::string rootOptStr = root.str();
    for (auto prop : props) {

      if (prop.first == rootStr || prop.first.find(rootOptStr) != std::string::npos) {
        removeProperty(prop.first, nm);
      }
    }
  }
}

}

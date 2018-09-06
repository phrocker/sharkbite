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

#ifndef TRANSPORTPOOL_H
#define TRANSPORTPOOL_H

#include "transport/BaseTransport.h"
#include "InterConnect.h"
#include <sstream>
#include <algorithm>
#include <random>

#include "transport/ServerConnection.h"
#include "transport/CachedTransport.h"

#include <map>
#include <vector>
#include <memory>
#include <set>
#include <mutex>
#include <arpa/inet.h>

#include <boost/shared_ptr.hpp>

namespace interconnect {
/**
 * Transport mechanism
 **/
template<typename Tr>
class TransportPool {
 public:
  TransportPool();

  ~TransportPool();

  /**
   * Frees the cached transport
   * @param cachedTransport is the cached transport mechanism
   **/
  void freeTransport(std::shared_ptr<CachedTransport<Tr>> cachedTransport);

  /**
   * Obtains a transport if one is available and caching is preferred. Otherwise
   * a new one will be created.
   * @param servers servers to connect to
   * @param preferCachedConnection prefer cached connections
   * @returns server to transport mapping.
   **/
  std::pair<std::string, std::shared_ptr<CachedTransport<Tr>>> getTransporter(const std::vector<std::shared_ptr<ServerConnection>> *servers, const bool preferCachedConnection);

  std::shared_ptr<CachedTransport<Tr>> getTransporter(std::shared_ptr<ServerConnection> conn) {
    std::vector<std::shared_ptr<ServerConnection>> servers;
    servers.push_back(conn);
    std::pair<std::string, std::shared_ptr<CachedTransport<Tr>>> cached = getTransporter(&servers, true);
    return cached.second;
  }

  /**
   * Close all cached connections.
   **/
  void closeAll() {
    if (!closed) {
      std::lock_guard < std::recursive_mutex > lock(cacheLock);


      if (!closed) {
        closing = true;
        for (auto ent1 : cache) {
          for (auto sec : ent1.second) {
            sec->close();
          }
        }
      }
      cache.clear();
      closed = true;

    }
  }

 protected:

  volatile bool closed;
  volatile bool closing;
  /**
   * Creates a new transport
   * @param conn server to which we will create a new transport
   * @returns new cached transport
   **/
  std::shared_ptr<CachedTransport<Tr>> createNewTransport(std::shared_ptr<ServerConnection> conn);

  std::recursive_mutex cacheLock;

  std::map<std::shared_ptr<ServerConnection>, std::vector<std::shared_ptr<CachedTransport<Tr>>>> cache;
  std::map<std::shared_ptr<ServerConnection>, uint32_t> errorCount;
  std::map<std::shared_ptr<ServerConnection>, uint32_t> errorTime;
  std::set<std::shared_ptr<ServerConnection>> badServers;
};

template<typename Tr>
TransportPool<Tr>::TransportPool()
    : closed(false),
      closing(false) {
  //cache = new std::map<std::shared_ptr<ServerConnection>, std::vector<CachedTransport<Tr>*>>();
}

template<typename Tr>
TransportPool<Tr>::~TransportPool() {
  if (!cache.empty())  //cache != NULL)
  {
    closeAll();
    //  delete cache;
//    cache= NULL;
  }
}

template<typename Tr>
void TransportPool<Tr>::freeTransport(std::shared_ptr<CachedTransport<Tr>> cachedTransport) {

  if (nullptr == cachedTransport || closed)
    return;

  //std::vector<CachedTransport<Tr>*> *closeList =
  //  new std::vector<CachedTransport<Tr>*>();
  std::vector<std::shared_ptr<CachedTransport<Tr>>> closeList;
  std::lock_guard < std::recursive_mutex > lock(cacheLock);

  if (closing || closed) {

    return;
  }

  auto cacheKey = cachedTransport->getCacheKey();

  std::vector<std::shared_ptr<CachedTransport<Tr>>> cachedConnections = cache.at(cacheKey);
  typename std::vector<std::shared_ptr<CachedTransport<Tr>>>::iterator cacheIter = cachedConnections.begin();

  timeval time;
  gettimeofday(&time, NULL);
  long millis = (time.tv_sec * 1000) + (time.tv_usec / 1000);
  for (; cacheIter != cachedConnections.end(); cacheIter++) {
    if (std::addressof(*((*cacheIter).get())) == std::addressof(*(cachedTransport.get()))) {
      if (cachedTransport->hasError()) {

        uint32_t errors = 0;

        if (errorCount.find(cacheKey) != errorCount.end()) {
          errors = errorCount.at(cacheKey);
        }

        errors++;

        errorCount[cacheKey] = errors;

        errorTime[cacheKey] = millis;

        if (errors > ERROR_THRESHOLD && badServers.find(cacheKey) == badServers.end()) {
          badServers.insert(cacheKey);
        }

      } else {
      }
      (*cacheIter)->setReturnTime(millis);
      (*cacheIter)->reserve(false);
      break;

    }
  }

  if (cachedTransport->hasError()) {
    cacheIter = cachedConnections.begin();
    for (; cacheIter != cachedConnections.end();) {
      if (!(*cacheIter)->isReserved()) {
        closeList.push_back(*cacheIter);
        cacheIter = cachedConnections.erase(cacheIter);
      } else {
        cacheIter++;
      }
    }

  }

  for (auto closer : closeList) {
    closer->close();
  }

  //delete closeList;

}

template<typename Tr>
std::pair<std::string, std::shared_ptr<CachedTransport<Tr>>> TransportPool<Tr>::getTransporter(const std::vector<std::shared_ptr<ServerConnection>> *servers, const bool preferCachedConnection) {

  if (preferCachedConnection) {
    std::lock_guard < std::recursive_mutex > lock(cacheLock);

    std::set<std::shared_ptr<ServerConnection>> serverSet(servers->begin(), servers->end());
    /*typename std::map<ServerConnection, std::vector<CachedTransport<Tr>*>>::iterator it =
     cache.begin();
     for (; it != cache.end(); it++) {
     ServerConnection conn = it->first;
     for(auto serv : *servers)
     {
     if (conn == serv)
     serverSet.insert(conn);
     }
     }*/
    if (!IsEmpty(&serverSet)) {

      std::vector<std::shared_ptr<ServerConnection>> connections(std::begin(serverSet), std::end(serverSet));
      auto engine = std::default_random_engine { };

      std::shuffle(connections.begin(), connections.end(), engine);

      for (std::shared_ptr<ServerConnection> conn : connections) {
        std::vector<std::shared_ptr<CachedTransport<Tr>>> cachedConnections = cache[conn];
        for (std::shared_ptr<CachedTransport<Tr>> cacheTransport : cachedConnections) {
          if (!cacheTransport->isReserved() && !cacheTransport->hasError() && cacheTransport->getCacheKey() == conn) {

            cacheTransport->reserve();
            std::stringstream hostname;
            hostname << conn->getHost() << ":" << conn->getPort();

            return std::make_pair(hostname.str(), cacheTransport);
          } else {

          }
        }
      }

    }

  }

  std::vector<std::shared_ptr<ServerConnection>> serverPool(*servers);

  short retryCount = 0;

  std::shared_ptr<ServerConnection> conn;
  while (serverPool.size() > 0 && retryCount < 10) {
    int index = std::rand() % serverPool.size();
    conn = serverPool.at(index);
    if (preferCachedConnection) {
      std::lock_guard < std::recursive_mutex > lock(cacheLock);

      std::vector<std::shared_ptr<CachedTransport<Tr>>> cachedConnections = cache[conn];
      if (!cachedConnections.empty()) {
        for (std::shared_ptr<CachedTransport<Tr>> cacheTransport : cachedConnections) {
          if (!cacheTransport->isReserved() && !cacheTransport->hasError() && cacheTransport->getCacheKey() == conn) {
            cacheTransport->reserve();
            std::stringstream hostname;
            hostname << conn->getHost() << ":" << conn->getPort();

            return std::make_pair(hostname.str(), cacheTransport);
          }
        }
      }

    }

    try {
      std::stringstream hostname;
      hostname << conn->getHost() << ":" << conn->getPort();
      std::pair<std::string, std::shared_ptr<CachedTransport<Tr>>> pair = std::make_pair(hostname.str(), createNewTransport(conn));

      return pair;
    } catch (std::runtime_error &tfe) {
      serverPool.erase(serverPool.begin() + index);
      retryCount++;
    }

  }

  throw std::runtime_error("Failed to connect to server");
}

template<typename Tr>
std::shared_ptr<CachedTransport<Tr>> TransportPool<Tr>::createNewTransport(std::shared_ptr<ServerConnection> conn) {

  std::lock_guard < std::recursive_mutex > lock(cacheLock);
  ;

  auto t = std::make_shared<Tr>(conn);

  auto cachedTransport = std::make_shared<CachedTransport<Tr>>(t, conn);

  cachedTransport->reserve();

  try {
    //cache[conn].push_back(cachedTransport);
    auto cachedConnections = cache[conn];

    cachedConnections.push_back(cachedTransport);

    cache[conn] = cachedConnections;
//    std::vector<std::shared_ptr<CachedTransport<Tr>> *cachedConnections = &cache.at(conn);

    //  cachedConnections.push_back(cachedTransport);
  } catch (const std::out_of_range& ex) {
    std::vector<std::shared_ptr<CachedTransport<Tr>>> cachedConnections;

    cachedConnections.push_back(cachedTransport);

    cache.insert(std::make_pair(conn, cachedConnections));

  }

  return cachedTransport;

}

}

#endif 

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

#include <arpa/inet.h>

#include <algorithm>
#include <map>
#include <memory>
#include <mutex>
#include <random>
#include <set>
#include <sstream>
#include <vector>

#include "InterConnect.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"
#include "transport/BaseTransport.h"
#include "transport/CachedTransport.h"
#include "transport/ServerConnection.h"

namespace interconnect {
/**
 * Transport mechanism
 **/
template <typename Tr>
class TransportPool {
 private:
  std::shared_ptr<logging::Logger> logger;

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
  std::pair<std::string, std::shared_ptr<CachedTransport<Tr>>> getTransporter(
      const std::vector<std::shared_ptr<ServerConnection>> *servers,
      const bool preferCachedConnection);

  std::shared_ptr<CachedTransport<Tr>> getTransporter(
      std::shared_ptr<ServerConnection> conn) {
    std::vector<std::shared_ptr<ServerConnection>> servers;
    servers.push_back(conn);
    std::pair<std::string, std::shared_ptr<CachedTransport<Tr>>> cached =
        getTransporter(&servers, true);
    logging::LOG_TRACE(logger) << "Reserving" << cached.second->getCacheKey();
    return cached.second;
  }

  /**
   * Close all cached connections.
   **/
  void closeAll() {
    if (!closed) {
      std::lock_guard<std::recursive_mutex> lock(cacheLock);

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
  std::shared_ptr<CachedTransport<Tr>> createNewTransport(
      std::shared_ptr<ServerConnection> conn);

  std::recursive_mutex cacheLock;

  std::map<std::shared_ptr<ServerConnection>,
           std::vector<std::shared_ptr<CachedTransport<Tr>>>,
           Cmp_ServerConnectionSP>
      cache;
  std::map<std::shared_ptr<ServerConnection>, uint32_t> errorCount;
  std::map<std::shared_ptr<ServerConnection>, uint32_t> errorTime;
  std::set<std::shared_ptr<ServerConnection>> badServers;
};

template <typename Tr>
TransportPool<Tr>::TransportPool()
    : closed(false),
      closing(false),
      logger(logging::LoggerFactory<TransportPool>::getLogger()) {
  // cache = new std::map<std::shared_ptr<ServerConnection>,
  // std::vector<CachedTransport<Tr>*>>();
}

template <typename Tr>
TransportPool<Tr>::~TransportPool() {
  if (!cache.empty())  // cache != NULL)
  {
    closeAll();
    //  delete cache;
    //    cache= NULL;
  }
}

template <typename Tr>
void TransportPool<Tr>::freeTransport(
    std::shared_ptr<CachedTransport<Tr>> cachedTransport) {
  if (nullptr == cachedTransport) {
    return;
  }

  auto cacheKey = cachedTransport->getCacheKey();

  logging::LOG_TRACE(logger) << "Freeing transport " << cacheKey;

  std::vector<std::shared_ptr<CachedTransport<Tr>>> closeList;
  std::lock_guard<std::recursive_mutex> lock(cacheLock);

  if (closing || closed) {
    logging::LOG_TRACE(logger) << "Closed or closing, so returning" << cacheKey;
    cachedTransport->close();
    return;
  }

  auto cachedConnections = cache.at(cacheKey);
  typename std::vector<std::shared_ptr<CachedTransport<Tr>>>::iterator
      cacheIter = cachedConnections.begin();

  timeval time;
  gettimeofday(&time, NULL);
  long millis = (time.tv_sec * 1000) + (time.tv_usec / 1000);
  bool foundCacheKey = false;

  for (; cacheIter != cachedConnections.end(); cacheIter++) {
    if (std::addressof(*((*cacheIter).get())) ==
        std::addressof(*(cachedTransport.get()))) {
      if (cachedTransport->hasError()) {
        uint32_t errors = 0;

        if (errorCount.find(cacheKey) != errorCount.end()) {
          errors = errorCount.at(cacheKey);
        }

        errors++;

        errorCount[cacheKey] = errors;

        errorTime[cacheKey] = millis;

        if (errors > ERROR_THRESHOLD &&
            badServers.find(cacheKey) == badServers.end()) {
          badServers.insert(cacheKey);
        }

      } else {
      }
      (*cacheIter)->setReturnTime(millis);
      logging::LOG_TRACE(logger) << "Cache Key is not reserved " << cacheKey;
      (*cacheIter)->reserve(false);
      foundCacheKey = true;
      break;
    }
  }

  if (cachedTransport->hasError()) {
    logging::LOG_TRACE(logger) << "Transport has an error " << cacheKey;
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

  if (!foundCacheKey) {
    logging::LOG_TRACE(logger) << "Closing transport" << cacheKey;
    cachedTransport->close();
  }

  for (auto closer : closeList) {
    closer->close();
  }
}

template <typename Tr>
std::pair<std::string, std::shared_ptr<CachedTransport<Tr>>>
TransportPool<Tr>::getTransporter(
    const std::vector<std::shared_ptr<ServerConnection>> *servers,
    const bool preferCachedConnection) {
  if (preferCachedConnection) {
    std::lock_guard<std::recursive_mutex> lock(cacheLock);

    std::set<std::shared_ptr<ServerConnection>> serverSet(servers->begin(),
                                                          servers->end());

    if (!IsEmpty(&serverSet)) {
      std::vector<std::shared_ptr<ServerConnection>> connections(
          std::begin(serverSet), std::end(serverSet));
      auto engine = std::default_random_engine{};

      std::shuffle(connections.begin(), connections.end(), engine);

      logging::LOG_TRACE(logger) << "Searching for cached connection "
                                 << connections.size() << " " << cache.size();

      for (std::shared_ptr<ServerConnection> conn : connections) {
        std::vector<std::shared_ptr<CachedTransport<Tr>>> cachedConnections =
            cache[conn];
        logging::LOG_TRACE(logger)
            << "Checking " << cachedConnections.size() << " connections";
        for (std::shared_ptr<CachedTransport<Tr>> cacheTransport :
             cachedConnections) {
          if (!cacheTransport->isReserved() && !cacheTransport->hasError() &&
              (*cacheTransport->getCacheKey().get() == *conn.get())) {
            logging::LOG_TRACE(logger)
                << "Returning " << conn->toString() << " via cache key "
                << cacheTransport->getCacheKey();
            cacheTransport->reserve();
            return std::make_pair(conn->toString(), cacheTransport);
          } else {
            if (cacheTransport->isReserved())
              logging::LOG_TRACE(logger)
                  << cacheTransport->getCacheKey()->getHost() << ":"
                  << cacheTransport->getCacheKey()->getPort() << " is reserved";
          }
        }
      }
    }
  }
  logging::LOG_TRACE(logger) << "Continuing to create a new connection";
  std::vector<std::shared_ptr<ServerConnection>> serverPool(*servers);

  short retryCount = 0;

  std::shared_ptr<ServerConnection> conn;
  while (serverPool.size() > 0 && retryCount < 10) {
    logging::LOG_TRACE(logger)
        << "Server pool is " << serverPool.size() << " " << retryCount;
    int index = std::rand() % serverPool.size();
    conn = serverPool.at(index);
    if (preferCachedConnection) {
      std::lock_guard<std::recursive_mutex> lock(cacheLock);

      std::vector<std::shared_ptr<CachedTransport<Tr>>> cachedConnections =
          cache[conn];
      if (!cachedConnections.empty()) {
        for (std::shared_ptr<CachedTransport<Tr>> cacheTransport :
             cachedConnections) {
          if (!cacheTransport->isReserved() && !cacheTransport->hasError() &&
              (*cacheTransport->getCacheKey().get() == *conn.get())) {
            cacheTransport->reserve();
            return std::make_pair(conn->toString(), cacheTransport);
          }
        }
      }
    }

    try {
      std::pair<std::string, std::shared_ptr<CachedTransport<Tr>>> pair =
          std::make_pair(conn->toString(), createNewTransport(conn));

      return pair;
    } catch (std::runtime_error &tfe) {
      serverPool.erase(serverPool.begin() + index);
      retryCount++;
    }
  }

  throw std::runtime_error("Failed to connect to server");
}

template <typename Tr>
std::shared_ptr<CachedTransport<Tr>> TransportPool<Tr>::createNewTransport(
    std::shared_ptr<ServerConnection> conn) {
  std::lock_guard<std::recursive_mutex> lock(cacheLock);

  auto t = std::make_shared<Tr>(conn);

  auto cachedTransport = std::make_shared<CachedTransport<Tr>>(t, conn);

  cachedTransport->reserve();

  logging::LOG_TRACE(logger)
      << "Creating a new connection to " << conn->getHost();

  try {
    auto cachedConnections = cache[conn];

    cachedConnections.push_back(cachedTransport);

    logging::LOG_TRACE(logger)
        << "Found a cache key " << cachedConnections.size();

    cache[conn] = cachedConnections;

  } catch (const std::out_of_range &ex) {
    std::vector<std::shared_ptr<CachedTransport<Tr>>> cachedConnections;

    cachedConnections.push_back(cachedTransport);

    cache.insert(std::make_pair(conn, cachedConnections));
  }

  return cachedTransport;
}

}  // namespace interconnect

#endif

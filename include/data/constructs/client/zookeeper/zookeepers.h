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
#ifndef ZOOKEEPERS_H
#define ZOOKEEPERS_H

#include <string>
#include <sstream>
#include <map>
#include <cstdint>
#include <vector>
#include <cstring>
#include <chrono>
#include <thread>
#include <sys/select.h>
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"
#include "watch.h"

#include <mutex>

#define  TIME_BETWEEN_CONNECT_CHECKS_MS  100
#define  TOTAL_CONNECT_TIME_WAIT_MS  10 * 1000

#define ZROOT "/accumulo"
#define ZINSTANCES "/instances"
#define ZMASTERS "/masters"
#define ZMASTER_LOCK "/masters/lock"
#define ZROOT_TABLET "/root_tablet"
#define ZROOT_TABLET_LOCATION "/root_tablet/location"
#define TSERVERS "/tservers"

namespace cclient {
namespace data {
namespace zookeeper {

static std::mutex syncBarrier;

static void watcher_function(zhandle_t*, int type, int state, const char *path, void *v) {

  Watch *ctx = ((WatchFn*) v)->ptr;

  if (state == ZOO_CONNECTED_STATE) {
    ctx->setConnected(true);
  } else {
    ctx->setConnected(false);
  }

  ctx->setState(state);

  if (type != ZOO_SESSION_EVENT) {
    Event event;
    event.path = path;
    event.type = type;
    ctx->pushEvent(event);
  }
}

class ZooKeeper {
 private: 
  std::shared_ptr<logging::Logger> logger;
 public:
  explicit ZooKeeper(const char *hostPorts, uint32_t timeout)
      : logger(logging::LoggerFactory<ZooKeeper>::getLogger()),
      hostPorts((char*) hostPorts),
      timeout(timeout),
      zookeeperReference(0),
      initWatchFp(0),
      myWatch(nullptr) {

  }

  virtual ~ZooKeeper() {
    if (NULL != zookeeperReference) {
      zookeeper_close(zookeeperReference);
    }
    if (NULL != initWatchFp) {
      delete initWatchFp;
    }
  }
  void setWatch(Watch *watch) {
    myWatch = watch;
  }

  bool isConnected() {
    if (!myWatch->isConnected()){
      logging::LOG_TRACE(logger) << "Failed to connect to " << hostPorts;
    }
    return myWatch->isConnected();
  }

  bool exists(std::string path, watcher_fn fn, WatchFn *ptr = NULL) {
    Stat stat = { 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 };

    // watcher will execute our function pointer
    if (ptr != NULL && ptr->ptr != myWatch) {
      ptr->ptr = myWatch;
    } else {
      fn = NULL;
    }

    zoo_wexists(zookeeperReference, path.c_str(), fn, ptr, &stat);

    return !stat_empty(&stat);
  }

  char* getData(std::string path, watcher_fn fn, WatchFn *ptr = NULL) {
    Stat stat = { 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 };

    // watcher will execute our function pointer

    if (ptr != NULL && ptr->ptr != myWatch) {
      ptr->ptr = myWatch;
    } else {
      fn = NULL;
    }

    int len = 1024;
    char *buffer = new char[len];
    memset(buffer, 0x00, len);

    zoo_wget(zookeeperReference, path.c_str(), fn, ptr, buffer, &len, &stat);

    if (len == -1) {
      delete[] buffer;
      buffer = NULL;
    }

    return buffer;
  }

  std::vector<std::string>* getChildren(std::string path, watcher_fn fn, WatchFn *ptr = NULL) {
    struct String_vector str_vec = { 0, 0 };

    // watcher will execute our function pointer

    if (ptr != NULL && ptr->ptr != myWatch) {
      ptr->ptr = myWatch;
    } else {
      fn = NULL;
    }

    zoo_wget_children(zookeeperReference, path.c_str(), fn, ptr, &str_vec);

    // add the children paths iff we have results
    std::vector<std::string> *newList = 0;

    if (str_vec.count > 0) {
      newList = new std::vector<std::string>();
      for (int i = 0; i < str_vec.count; i++) {
        newList->push_back(std::string(str_vec.data[i]));
      }
    }
    deallocate_String_vector(&str_vec);

    return newList;
  }

  void init(Watch *watch) {
    FILE* outfile =  fopen ("nul", "w");
    zoo_set_log_stream(outfile);
    myWatch = watch;
    // init zk with our zk info, and the watcher_function will
    // label our watch as 'connected'
    initWatchFp = new WatchFn();
    initWatchFp->ptr = myWatch;
    zookeeperReference = zookeeper_init(hostPorts, watcher_function, timeout, 0, initWatchFp, 0);
    
    myWatch->setHandle(zookeeperReference);
    logging::LOG_TRACE(logger) << "Connecting to " << hostPorts;

  }

 protected:

  inline static bool stat_empty(struct Stat *a) {
    Stat statB = { 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 };

    return (stat_eq(a, &statB) == 1);
  }

  inline static int stat_eq(struct Stat *a, struct Stat *b) {
    if (a->czxid != b->czxid)
      return 0;
    if (a->mzxid != b->mzxid)
      return 0;
    if (a->ctime != b->ctime)
      return 0;
    if (a->mtime != b->mtime)
      return 0;
    if (a->version != b->version)
      return 0;
    if (a->cversion != b->cversion)
      return 0;
    if (a->aversion != b->aversion)
      return 0;
    if (a->ephemeralOwner != b->ephemeralOwner)
      return 0;
    if (a->dataLength != b->dataLength)
      return 0;
    if (a->numChildren != b->numChildren)
      return 0;
    if (a->pzxid != b->pzxid)
      return 0;
    return 1;
  }

  char *hostPorts;
  uint32_t timeout;
  zhandle_t *zookeeperReference;
  Watch *myWatch;
  WatchFn *initWatchFp;

};

class ZooSession {
 public:
  ZooSession(ZooKeeper *keeper, Watch *watcher)
      :
      zooKeeper(keeper) {

    keeper->init(watcher);

  }

  ZooSession(ZooKeeper *keeper)
      :
      zooKeeper(keeper) {
    // already initialized session

  }

  ZooKeeper* getZooKeeper() {
    return zooKeeper;
  }

 protected:
  ZooKeeper *zooKeeper;
};

class ZooKeepers {
 private:
  static std::map<std::string, ZooSession*> sessions;
  
 public:

  static std::string sessionKey(std::string keepers, uint16_t timeout, std::string auth) {
    std::stringstream ss(std::stringstream::in | std::stringstream::out);
    ss << keepers << ":" << timeout << ":" << (auth.size() > 0 ? auth : "");
    return ss.str();
  }

  static ZooSession* getSession(std::string zookeepers, uint16_t timeout, std::string auth) {
    std::lock_guard<std::mutex> lock(syncBarrier);
    std::string sessionKey = ZooKeepers::sessionKey(zookeepers, timeout, auth);
    std::string readOnlyKey = ZooKeepers::sessionKey(zookeepers, timeout, "");

    std::map<std::string, ZooSession*>::iterator it = sessions.find(sessionKey);

    // it exists

    ZooSession *zsi = NULL;

    if (it != sessions.end()) {
      zsi = (*it).second;
      if (!zsi->getZooKeeper()->isConnected()) {
        std::map<std::string, ZooSession*>::iterator pt;
        if ((pt = sessions.find(readOnlyKey)) == it) {
          sessions.erase(pt);
        }
        sessions.erase(it);
      }

    } else {
      Watch *watcher = new Watch();

      zsi = new ZooSession(connect(zookeepers, timeout, auth, watcher));

      sessions.insert(std::make_pair(sessionKey, zsi));
    }

    return zsi;
  }

  static ZooKeeper* connect(std::string &hosts, uint32_t timeout, std::string auth, Watch *watcher) {
    bool tryAgain = true;
    ZooKeeper *zk = NULL;
    do {
      zk = new ZooKeeper(hosts.c_str(), timeout);

      zk->init(watcher);

      for (int i = 0; i < TOTAL_CONNECT_TIME_WAIT_MS / TIME_BETWEEN_CONNECT_CHECKS_MS && tryAgain; i++) {

        if (zk->isConnected()) {
          
          if (auth.size() > 0) {
            break;
            tryAgain = false;
          } else {
            // sleep
            std::this_thread::sleep_for(std::chrono::milliseconds(1000));
          }
        }
      }
      //else if (ret == ReturnCode::type::

    } while (tryAgain);

    return zk;
  }
};
}
}
}
//const std::map<string,

#endif // ZOOKEEPERS_H

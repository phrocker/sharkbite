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

#ifndef ZOOCACHE_H
#define ZOOCACHE_H

#include <iterator>
#include <algorithm>
#include <string>
#include <set>
#include <map>
#include <vector>
#include <initializer_list>

#include "../../inputvalidation.h"
#include "../InstanceCache.h"
#include "zookeepers.h"
#include "watch.h"

#include <time.h>
#include <mutex>

namespace cclient
{
  namespace data
  {
    namespace zookeeper
    {

      /**
 Zoo cache that mimics the one within Accumulo
 **/
      class ZooCache : public cclient::data::InstanceCache
      {
      public:
        explicit ZooCache(ZooKeeper *zk)
            : myZk(zk)
        {
        }

        ~ZooCache()
        {

          clear();
          std::lock_guard<std::mutex> lock(syncBarrier);
          fns.clear();
        }

        inline static void cache_watcher(zhandle_t *, int type, int state, const char *path, void *v)
        {
          WatchFn *ctx = (WatchFn *)v;
          ZooCache *cacheRef = (ZooCache *)ctx->Fn;
          Event event;
          event.path = path;
          event.type = type;
          if (type == ZOO_CHANGED_EVENT || type == ZOO_CHILD_EVENT || type == ZOO_CREATED_EVENT || type == ZOO_DELETED_EVENT)
          {
            cacheRef->removePath(&event);
          }
          else if (type == ZOO_SESSION_EVENT)
          {
            if (state != ZOO_CONNECTED_STATE)
            {
              cacheRef->clear();
            }
          }
          else
          {
            // do nothing
          }
        }

        uint8_t *getData(std::string path)
        {
          if (IsEmpty(&path))
          {
            return NULL;
          }
          std::lock_guard<std::mutex> lock(syncBarrier);

          std::map<std::string, uint8_t *>::iterator cachedData = cache.find(path);

          if (cachedData == cache.end())
          {

            auto watchFp = std::make_shared<WatchFn>();
            fns.insert(watchFp);
            watchFp->Fn = this;
            if (myZk->exists(path, cache_watcher, watchFp))
            {
              watchFp = std::make_shared<WatchFn>();
              fns.insert(watchFp);
              watchFp->Fn = this;

              uint8_t *data = (uint8_t *)myZk->getData(path, cache_watcher, watchFp);
              cache.insert(std::pair<std::string, uint8_t *>(std::string(path), (uint8_t *)data));

              return data;
            }
            else
            {

              return NULL;
            }

            cachedData = cache.find(path);
          }

          uint8_t *ptr = cachedData->second;

          return ptr;
        }

        std::vector<std::string> getChildren(const std::string path, bool force = false)
        {
          if (IsEmpty(&path))
          {
            return std::vector<std::string>();
          }

          std::lock_guard<std::mutex> lock(syncBarrier);

          std::map<std::string, std::vector<std::string> *>::iterator children = childrenCache.find(path);

          if (force || children == childrenCache.end())
          {
            std::shared_ptr<WatchFn> watchFp = std::make_shared<WatchFn>();
            fns.insert(watchFp);
            watchFp->Fn = this;

            std::vector<std::string> *results = myZk->getChildren(path, cache_watcher, watchFp);

            if (NULL != results)
            {
              childrenCache.insert(std::pair<std::string, std::vector<std::string> *>(std::string(path), results));
            }

            children = childrenCache.find(path);
          }

          if (children == childrenCache.end())
          {
            return std::vector<std::string>();
          }
          std::vector<std::string> strings(*children->second);

          return strings;
        }

      protected:
        void removePath(Event *event)
        {
          if (NULL == event || IsEmpty(&event->path))
          {
            return;
          }
          std::lock_guard<std::mutex> lock(syncBarrier);
          // critical section

          std::map<std::string, uint8_t *>::iterator find = cache.find(event->path);
          if (find != std::end(cache))
          {
            uint8_t *prevMemory = find->second;
            cache.erase(find);
            delete[] prevMemory;
          }
          std::map<std::string, std::vector<std::string> *>::iterator childFind = childrenCache.find(event->path);
          if (childFind != std::end(childrenCache))
          {
            std::vector<std::string> *children = childFind->second;
            childrenCache.erase(childFind);
            delete children;
          }

        }

        void clear()
        {

          std::lock_guard<std::mutex> lock(syncBarrier);
          // critical section

          for (auto it = cache.begin(); it != cache.end(); it++)
          {
            delete[] it->second;
          }

          cache.clear();

          for (auto it = childrenCache.begin(); it != childrenCache.end(); it++)
          {
            delete it->second;
          }

          childrenCache.clear();
        }

        std::set<std::shared_ptr<WatchFn>> fns;
        ZooKeeper *myZk;
        std::map<std::string, uint8_t *> cache;
        std::map<std::string, std::vector<std::string> *> childrenCache;
        std::mutex syncBarrier;
      };

    } // namespace zookeeper
  }   // namespace data
} // namespace cclient
#endif // ZOOCACHE\_H

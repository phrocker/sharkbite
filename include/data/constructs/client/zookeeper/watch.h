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
#ifndef WATCH_H
#define WATCH_H

#include <iostream>
#include <string>
//#include <zookeeper/recordio.h>
#include <zookeeper/zookeeper.h>
#include <zookeeper/zookeeper.jute.h>
#include <list>
#include <sys/select.h>
#include <thread>
#include <pthread.h>

using namespace std;

class Event
{
public:
    std::string path;
    int type;
};

namespace cclient
{
    namespace data
    {
        namespace zookeeper
        {

            class Watch
            {
            public:
                Watch() : connected(false), zookeeperReference(NULL), state(0)
                {

                    pthread_mutex_init(&mutex, 0);
                }
                ~Watch()
                {
                    pthread_mutex_destroy(&mutex);
                }

                Event popEvent();

                void setHandle(zhandle_t *zh);

                bool isConnected() const
                {
                    return connected;
                }

                bool setConnected(bool val)
                {
                    connected = val;
                    return connected;
                }

                int size() const;

                void pushEvent(Event event);

                bool waitForConnected(zhandle_t *zh);

                bool waitForDisconnected(zhandle_t *zh);

                void yield(zhandle_t *zh, int i)
                {
                    std::this_thread::yield();
                }

                void setState(int st)
                {
                    state = st;
                }

                int getState()
                {
                    return state;
                }

            protected:
                list<Event> events;
                bool connected;
                int state;
                zhandle_t *zookeeperReference;

            private:
                mutable pthread_mutex_t mutex;
            };

            struct WatchFn
            {
                Watch *ptr;
                void *Fn;
            };
        }; // namespace zookeeper
    }      // namespace data
} // namespace cclient
#endif // WATCH_H

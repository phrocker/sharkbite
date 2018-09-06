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

#include "data/constructs/client/zookeeper/watch.h"

using namespace cclient::data::zookeeper;

Event
Watch::popEvent ()
{
    Event my_event;

    pthread_mutex_lock (&mutex);
    my_event = events.front ();
    events.pop_back ();
    pthread_mutex_unlock (&mutex);
    return my_event;
}

int
Watch::size ()
{
    int size = 0;
    pthread_mutex_lock (&mutex);
    size = events.size ();
    pthread_mutex_unlock (&mutex);

    return size;
}

void
Watch::setHandle (zhandle_t *zh)
{
    zookeeperReference = zh;
}

void
Watch::pushEvent (Event event)
{
    pthread_mutex_lock (&mutex);
    events.push_back (event);
    pthread_mutex_unlock (&mutex);
}

bool
Watch::waitForConnected (zhandle_t *zh)
{
    time_t expires = time (0) + 10;
    while (!connected && time (0) < expires)
    {
        yield (zh, 1);
    }
    return connected;
}

bool
Watch::waitForDisconnected (zhandle_t *zh)
{
    time_t expires = time (0) + 15;
    while (connected && time (0) < expires)
    {
        yield (zh, 1);
    }
    return !connected;
}

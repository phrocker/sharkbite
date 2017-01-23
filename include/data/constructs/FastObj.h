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
#ifndef FAST_OBJ_H_
#define FAST_OBJ_H_


#include "../extern/cpp-btree/btree_set.h"
#include "../extern/cpp-btree/btree_map.h"
#include "../extern/concurrentqueue/concurrentqueue.h"

template<typename T, typename V>
struct GoogleMap
{

    typedef btree::btree_map<T,V> Map;

};

template<typename T>
struct GoogleSet
{
    typedef btree::btree_set<T> Set;
};

template<typename T>
struct ConcurrentQueue
{
    typedef moodycamel::ConcurrentQueue<T> Queue;
};

#endif



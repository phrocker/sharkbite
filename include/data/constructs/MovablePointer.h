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

#ifndef MOVABLE_PTR_H_
#define MOVABLE_PTR_H_
namespace cclient{
namespace data{
template<typename T>
struct movable_ptr
{
    T *pointer;
    movable_ptr(T *ptr=0) : pointer(ptr) {} 
    movable_ptr<T>& operator=(T *ptr) { pointer = ptr; return *this; }
    movable_ptr(movable_ptr<T> && other) 
    {
        pointer = other.pointer;
        other.pointer = 0;
    }
    movable_ptr<T>& operator=(movable_ptr<T> && other) 
    {
        pointer = other.pointer;
        other.pointer = 0;
        return *this;
    } 
    T* operator->() const { return pointer; }
    T& operator*() const { return *pointer; }

    movable_ptr(movable_ptr<T> const & other) = default;
    movable_ptr<T> & operator=(movable_ptr<T> const & other) = default;
};
}
}
#endif
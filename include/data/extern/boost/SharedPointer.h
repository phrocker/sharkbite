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


#pragma once

#include <memory>
#include <boost/concept_check.hpp>
#include <boost/shared_ptr.hpp>

namespace boost {
namespace tools {

template<class PtrConvert> struct Replacement {
  PtrConvert p;

  Replacement(const PtrConvert &p)
      : p(p) {
  }
  Replacement(const Replacement &other)
      : p(other.p) {
  }
  Replacement(Replacement &&other)
      : p(std::move(other.p)) {
  }

  void operator ()(...) {
    p.reset();
  }
};

template<class T> std::shared_ptr<T> to_shared_ptr(const boost::shared_ptr<T> &p) {
  typedef Replacement<std::shared_ptr<T>> H;
  if (H *h = boost::get_deleter < H > (p)) {
    return h->p;
  } else {
    return std::shared_ptr < T > (p.get(), Replacement<boost::shared_ptr<T>>(p));
  }
}

template<class T> boost::shared_ptr<T> from_shared_ptr(const std::shared_ptr<T> &p) {
  typedef Replacement<boost::shared_ptr<T>> H;
  if (H * h = std::get_deleter < H > (p)) {
    return h->p;
  } else {
    return boost::shared_ptr < T > (p.get(), Replacement<std::shared_ptr<T>>(p));
  }
}

}
}

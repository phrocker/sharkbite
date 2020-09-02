/**
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
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

#include <string>
#ifdef _WIN32
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN 1
#endif
// can't include cxxabi
#else
#include <cxxabi.h>
#endif

namespace ClassUtils {

template <typename T>
static inline std::string getClassName() {
#ifndef WIN32
  char *b = abi::__cxa_demangle(typeid(T).name(), 0, 0, 0);
  if (b == nullptr) return std::string();
  std::string name = b;
  std::free(b);
  return name;
#else
  std::string adjusted_name = typeid(T).name();
  // can probably skip class  manually for slightly higher performance
  const std::string clazz = "class ";
  auto haz_clazz = adjusted_name.find(clazz);
  if (haz_clazz == 0)
    adjusted_name = adjusted_name.substr(
        clazz.length(), adjusted_name.length() - clazz.length());
  return adjusted_name;
#endif
}

/**
 * Shortens class names via the canonical representation ( package with name )
 * @param class_name input class name
 * @param out output class name that is shortened.
 * @return true if out has been updated, false otherwise
 */
bool shortenClassName(const std::string &class_name, std::string &out);

} /* namespace ClassUtils */

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

#ifndef CONFIGURATION_H_
#define CONFIGURATION_H_

#include <memory>
#include <string>
#include <map>

namespace cclient {
namespace impl {

/**
 * Purpose & Design: Based on the hadoop configuration object
 */
class Configuration : public std::enable_shared_from_this<Configuration> {
 public:
  Configuration() {
  }
  /**
   * Constructor
   */
  virtual ~Configuration();
  /**
   * Sets the value of the name
   * @param name config option name
   * @param value
   */
  void set(const std::string &name, const std::string &value);
  /**
   * Returns the config option
   * @param name
   */
  std::string get(const std::string &name) const;

  /**
   * Returns the config option using default if the map option isn't set
   */
  std::string get(const std::string &name, const std::string &def) const;

  uint32_t getLong(const std::string &name) const;
  uint32_t getLong(const std::string &name, uint32_t def) const;

 protected:
  std::map<std::string, std::string> configurationMap;
};

} /* namespace impl */
} /* namespace cclient */
#endif /* CONFIGURATION_H_ */

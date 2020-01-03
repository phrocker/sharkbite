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

#ifndef INCLUDE_JNI_ACCUMULOKEY_H_
#define INCLUDE_JNI_ACCUMULOKEY_H_

namespace cclient {
namespace jni {

class AccumuloKey {
  jobject key;
  JNIEnv *env;
  mutable jclass keyClass;

  std::string row, cf, cq, cv;
  jlong ts = 0;
  bool deleted = false;
 public:
  AccumuloKey(JNIEnv *env, jobject key)
      :
      env(env),
      key(key) {
    keyClass = env->GetObjectClass(key);

  }

  bool init(JNIEnv *env) {
    /**
     * load compoonent parts
     */

    auto textClass = env->FindClass("org/apache/hadoop/io/Text");

    auto getRow = env->GetMethodID(keyClass, "getRow", "()Lorg/apache/hadoop/io/Text;");
    auto getColumnFamily = env->GetMethodID(keyClass, "getColumnFamily", "()Lorg/apache/hadoop/io/Text;");
    auto getColumnQualifier = env->GetMethodID(keyClass, "getColumnQualifier", "()Lorg/apache/hadoop/io/Text;");
    auto getColumnVisibility = env->GetMethodID(keyClass, "getColumnVisibility", "()Lorg/apache/hadoop/io/Text;");
    auto getTimestamp = env->GetMethodID(keyClass, "getTimestamp", "()J");

    auto jrow = env->CallObjectMethod(key, getRow);
    if (env->ExceptionCheck()) {
      return false;
    }
    auto jcf = env->CallObjectMethod(key, getColumnFamily);
    if (env->ExceptionCheck()) {
      return false;
    }
    auto jcq = env->CallObjectMethod(key, getColumnQualifier);
    if (env->ExceptionCheck()) {
      return false;
    }
    auto jcv = env->CallObjectMethod(key, getColumnVisibility);
    if (env->ExceptionCheck()) {
      return false;
    }
    ts = env->CallLongMethod(key, getTimestamp);
    if (env->ExceptionCheck()) {
      return false;
    }

    auto textToString = env->GetMethodID(textClass, "toString", "()Ljava/lang/String;");

    auto jrowstr = (jstring) env->CallObjectMethod(jrow, textToString);
    if (env->ExceptionCheck()) {
      return false;
    }
    auto jcfstr = (jstring) env->CallObjectMethod(jcf, textToString);
    if (env->ExceptionCheck()) {
      return false;
    }
    auto jcqstr = (jstring) env->CallObjectMethod(jcq, textToString);
    if (env->ExceptionCheck()) {
      return false;
    }
    auto jcvstr = (jstring) env->CallObjectMethod(jcv, textToString);
    if (env->ExceptionCheck()) {
      return false;
    }

    row = JniStringToUTF(env, jrowstr);
    cf = JniStringToUTF(env, jcfstr);
    cq = JniStringToUTF(env, jcqstr);
    cv = JniStringToUTF(env, jcvstr);
    return true;
  }

  AccumuloKey(const std::shared_ptr<cclient::data::Key> &key)
      :
      env(nullptr),
      key(nullptr) {

    row = key->getRowStr();
    cf = key->getColFamilyStr();
    cq = key->getColQualifierStr();
    cv = key->getColVisibilityStr();
    ts = key->getTimeStamp();

  }

  std::shared_ptr<cclient::data::Key> toKey() {
    auto key = std::make_shared<cclient::data::Key>();
    key->setRow(row);
    key->setColFamily(cf);
    key->setColQualifier(cq);
    key->setColVisibility(cv);
    int64_t lts = ts;
    if (lts != -1) {
      key->setTimeStamp(lts);
    }

    return key;
  }

  jobject getAccumuloKey(JNIEnv *env) const {
    keyClass = env->FindClass("org/apache/accumulo/core/data/Key");

    auto jrow = toByteArray(env, row);

    jlong timeStamp = ts;
    jobject jkey = nullptr;
    if (!cv.empty()) {
      auto jcf = toByteArray(env, cf);
      auto jcq = toByteArray(env, cq);
      auto jcv = toByteArray(env, cv);
      auto constructor = env->GetMethodID(keyClass, "<init>", "([B[B[B[BJ)V");
      jkey = env->NewObject(keyClass, constructor, jrow, jcf, jcq, jcv, timeStamp);
      env->DeleteLocalRef(jcf);
      env->DeleteLocalRef(jcq);
      env->DeleteLocalRef(jcv);
    } else if (!cq.empty()) {

      auto jcf = toByteArray(env, cf);
      auto jcq = toByteArray(env, cq);
      auto constructor = env->GetMethodID(keyClass, "<init>", "([B[B[B)V");
      jkey = env->NewObject(keyClass, constructor, jrow, jcf, jcq);
      env->DeleteLocalRef(jcf);
      env->DeleteLocalRef(jcq);
    } else if (!cf.empty()) {

      auto jcf = toByteArray(env, cf);
      auto constructor = env->GetMethodID(keyClass, "<init>", "([B[B)V");
      jkey = env->NewObject(keyClass, constructor, jrow, jcf);

      env->DeleteLocalRef(jcf);

    } else {
      auto constructor = env->GetMethodID(keyClass, "<init>", "([B)V");
      jkey = env->NewObject(keyClass, constructor, jrow);

    }

    env->DeleteLocalRef(jrow);

    return jkey;

  }

  std::string getRow() const {
    return row;
  }

  std::string getCf() const {
    return cf;
  }

  std::string getCq() const {
    return cq;
  }

  std::string getCv() const {
    return cv;
  }

  long getTimeStamp() const {
    return ts;
  }

};

} /** jni **/
} /** cclient **/

#endif /* INCLUDE_JNI_ACCUMULOKEY_H_ */

#include <memory>


namespace data{
namespace version1 {

class AccumuloMaster{


  void *getRaw() {
    return base_ptr.get();
  }

 private:

  std::unique_ptr<void> base_ptr_;
};

}
}

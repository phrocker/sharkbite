CPPFLAGS=-g -std=c++11 -lthrift -I/usr/local/include/thrift/ -I /usr/local/include/zookeeper -lpthread
LDFLAGS=-g -std=c++11 -lthrift -I/usr/local/include/thrift/ -lpthread
LDLIBS=
RM=rm -f

SRCS=watch.cpp zookeepers.cpp
OBJS=$(subst .cpp,.o,$(SRCS))


all: zookeeper

clean: 
	$(RM) $(OBJS)


zookeeper:	$(OBJS)
	g++ -o zookeeper.so $(OBJS) $(LDFLAGS) -shared 
	$(RM) $(OBJS)

watch.o: watch.cpp
	g++ $(CPPFLAGS) -c watch.cpp -fPIC
	
zookeepers.o: zookeepers.cpp
	g++ $(CPPFLAGS) -c zookeepers.cpp -fPIC

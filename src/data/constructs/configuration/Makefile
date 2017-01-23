CPPFLAGS=-g --std=c++11 -lthrift -I/usr/local/include/thrift/
LDFLAGS=-g
LDLIBS=
RM=rm -f

SRCS=Configuration.cpp
OBJS=$(subst .cpp,.o,$(SRCS))


all: Configuration

clean: 
	$(RM) $(OBJS)


Configuration:	$(OBJS)
	g++ -o configuration.so $(OBJS) -shared -fPIC
	$(RM) $(OBJS)

Configuration.o: Configuration.cpp
	g++ $(CPPFLAGS) -c Configuration.cpp -fPIC
